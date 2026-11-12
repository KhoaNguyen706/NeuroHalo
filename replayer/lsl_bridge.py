"""Forward a live EEG stream to the gateway as a session.

    uv run --group lsl lsl_bridge.py                      # the first EEG stream on the network
    uv run --group lsl lsl_bridge.py --name NeuroHalo-sim

Lab Streaming Layer (LSL) is how EEG amplifiers publish their samples on a
local network. This bridge finds such a stream and sends it on in 100 ms
batches, the same messages the replayer sends for a file. The session shows
up on the home page while it is live, and ends when the stream goes quiet.

If the network also has a marker stream (LSL type "Markers", which experiment
software such as PsychoPy publishes when it shows a cue), its markers are sent
along, each inside the batch that holds the sample it belongs to.

With no amplifier at hand, lsl_sim.py publishes a recording as an LSL stream.
"""

import argparse
import os
import threading
import time
import uuid
from collections import deque

import grpc
import numpy as np
import pylsl

from edf import _standard_names, clip_label, normalize_channel_name
from neuro.v1 import ingest_pb2, ingest_pb2_grpc
from replay import BATCH_SECONDS, RETRYABLE, REWIND_BATCHES, ReplayError

TO_MICROVOLTS = {"microvolts": 1.0, "microvolt": 1.0, "uv": 1.0, "µv": 1.0, "": 1.0,
                 "millivolts": 1e3, "mv": 1e3, "volts": 1e6, "v": 1e6}


def channel_layout(info: "pylsl.StreamInfo") -> tuple[list[str], float]:
    """Read the channel names and unit from a stream's description.

    Returns the names in standard spelling ("Ch1", "Ch2", ... when the stream
    does not name its channels) and the factor that turns its values into
    microvolts (1 when the unit is missing or unknown, which is LSL's usual
    unit for EEG).
    """
    standard = _standard_names()
    names: list[str] = []
    unit = ""
    channel = info.desc().child("channels").child("channel")
    while not channel.empty() and len(names) < info.channel_count():
        names.append(normalize_channel_name(channel.child_value("label"), standard))
        unit = unit or channel.child_value("unit")
        channel = channel.next_sibling("channel")
    if len(names) != info.channel_count() or not all(names):
        names = [f"Ch{i + 1}" for i in range(info.channel_count())]
    return names, TO_MICROVOLTS.get(unit.strip().lower(), 1.0)


# One live stream being forwarded: the inlet plus how far it has been sent,
# and the markers waiting to be sent with their samples.
# Created by: main.
# Used by: main, which calls requests() once per gateway connection.
class Bridge:
    def __init__(self, inlet: "pylsl.StreamInlet", session_id: str, idle_seconds: float, stop: threading.Event,
                 marker_inlet: "pylsl.StreamInlet | None" = None):
        """Read the stream's rate and channels, and work out the batch size.
        marker_inlet is the experiment's marker stream, when there is one."""
        info = inlet.info()
        self.inlet = inlet
        self.marker_inlet = marker_inlet
        self.pulled = 0  # samples taken from the EEG stream so far
        self.newest_stamp = 0.0  # LSL time of the newest of them
        self.waiting: list[tuple[float, str]] = []  # markers (LSL time, label) not yet in a batch
        self.late_markers = 0  # markers that arrived after their sample was sent
        self.session_id = session_id
        self.idle_seconds = idle_seconds
        self.stop = stop
        self.rate = info.nominal_srate()
        self.names, self.scale = channel_layout(info)
        self.batch_samples = round(self.rate * BATCH_SECONDS)
        self.next_seq = 0  # survives reconnects
        self.recent: deque = deque(maxlen=REWIND_BATCHES)  # sent lately, sent again after a reconnect
        self.pending = np.empty((0, len(self.names)), dtype=np.float32)  # samples not yet in a batch
        self.ended = False  # the stream went quiet, or Ctrl+C

    def requests(self):
        """Yield the messages for one gateway stream: the header, the batches
        of the last second again (some may have been lost when the previous
        stream broke; the processor drops the ones that did arrive), then a
        new batch for every 100 ms of samples the LSL stream delivers. Ends
        when no sample has arrived for idle_seconds, or on Ctrl+C."""
        yield ingest_pb2.IngestRequest(header=ingest_pb2.SessionHeader(
            session_id=self.session_id, sample_rate_hz=self.rate, channel_names=self.names, source="lsl"))
        yield from list(self.recent)
        last_sample = time.monotonic()
        while not self.stop.is_set():
            chunk, stamps = self.inlet.pull_chunk(timeout=0.2, max_samples=self.batch_samples * 10)
            self.pull_markers()
            if chunk:
                last_sample = time.monotonic()
                self.pulled += len(chunk)
                self.newest_stamp = stamps[-1]
                rows = np.asarray(chunk, dtype=np.float32) * self.scale
                self.pending = np.concatenate([self.pending, rows])
            elif time.monotonic() - last_sample > self.idle_seconds:
                break
            while len(self.pending) >= self.batch_samples:
                rows, self.pending = self.pending[: self.batch_samples], self.pending[self.batch_samples:]
                first = self.next_seq * self.batch_samples
                request = ingest_pb2.IngestRequest(batch=ingest_pb2.SampleBatch(
                    session_id=self.session_id,
                    seq=self.next_seq,
                    first_sample_index=first,
                    sent_time_unix_nanos=time.time_ns(),
                    num_samples=len(rows),
                    values=rows.ravel(),
                    markers=self.take_markers(first, first + len(rows)),
                ))
                self.recent.append(request)
                self.next_seq += 1
                yield request
        self.ended = True

    def pull_markers(self) -> None:
        """Move every marker that has arrived on the marker stream into the
        waiting list. Does nothing when there is no marker stream."""
        if self.marker_inlet is None:
            return
        samples, stamps = self.marker_inlet.pull_chunk(timeout=0.0)
        for sample, stamp in zip(samples, stamps):
            label = clip_label(str(sample[0]))
            if label:
                self.waiting.append((stamp, label))

    def take_markers(self, first: int, end: int) -> list[ingest_pb2.Marker]:
        """Return the waiting markers that belong in the batch holding samples
        first to end-1, and remove them from the waiting list.

        A marker's sample is found by counting back from the newest EEG sample:
        both streams carry LSL times on the same clock. A marker whose sample
        was already sent goes on this batch's first sample and is counted as
        late; one whose sample has not arrived yet keeps waiting."""
        markers, still_waiting = [], []
        for stamp, label in self.waiting:
            index = self.pulled - 1 - round((self.newest_stamp - stamp) * self.rate)
            if index >= end:
                still_waiting.append((stamp, label))
                continue
            if index < first:
                self.late_markers += 1
                index = first
            markers.append(ingest_pb2.Marker(sample_index=index, label=label))
        self.waiting = still_waiting
        return markers


def main() -> None:
    """Find the LSL stream, then forward it until it goes quiet, reconnecting
    to the gateway with backoff when the connection breaks."""
    p = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    p.add_argument("--name", default=None, help="LSL stream name; default: the first stream of type EEG")
    p.add_argument("--markers", default=None, metavar="NAME",
                   help='marker stream name; default: the first stream of type Markers, if any; "none" sends no markers')
    p.add_argument("--gateway", default="localhost:50051", help="gateway address")
    p.add_argument("--session-id", default=None, help="defaults to a random ID")
    p.add_argument("--wait", type=float, default=10.0, help="seconds to look for the stream")
    p.add_argument("--idle", type=float, default=5.0, help="seconds without samples before the session ends")
    p.add_argument("--token", default=os.environ.get("INGEST_TOKEN", "dev-token"))
    args = p.parse_args()

    prop, value = ("name", args.name) if args.name else ("type", "EEG")
    found = pylsl.resolve_byprop(prop, value, timeout=args.wait)
    if not found:
        raise SystemExit(f"no LSL stream with {prop} {value!r} found within {args.wait:g} s")
    # proc_ALL puts both streams' times on this machine's clock and smooths
    # them, so a marker's time can be compared with the samples' times.
    inlet = pylsl.StreamInlet(found[0], max_buflen=60, processing_flags=pylsl.proc_ALL)
    marker_inlet = None
    if args.markers != "none":
        prop_m, value_m = ("name", args.markers) if args.markers else ("type", "Markers")
        found_m = pylsl.resolve_byprop(prop_m, value_m, timeout=args.wait if args.markers else 2.0)
        if found_m:
            marker_inlet = pylsl.StreamInlet(found_m[0], max_buflen=60, processing_flags=pylsl.proc_ALL)
            print(f"markers from LSL stream {found_m[0].name()!r}")
        elif args.markers:
            raise SystemExit(f"no LSL marker stream named {args.markers!r} found within {args.wait:g} s")
    stop = threading.Event()
    bridge = Bridge(inlet, args.session_id or f"s-{uuid.uuid4().hex[:12]}", args.idle, stop, marker_inlet)
    if bridge.rate <= 0:
        raise SystemExit("the stream has no fixed sample rate; only regularly sampled streams can be forwarded")
    print(f"session {bridge.session_id}: LSL stream {found[0].name()!r}, {len(bridge.names)} channels at "
          f"{bridge.rate:g} Hz -> {args.gateway}")

    metadata = [("authorization", f"Bearer {args.token}")]
    backoff = 0.5
    try:
        while not bridge.ended:
            try:
                with grpc.insecure_channel(args.gateway) as channel:
                    summary = ingest_pb2_grpc.IngestStub(channel).Stream(bridge.requests(), metadata=metadata)
                print(f"session {bridge.session_id}: stream ended, {bridge.next_seq} batches sent, "
                      f"gateway's last seq {summary.last_seq}")
                if bridge.late_markers:
                    print(f"{bridge.late_markers} markers arrived after their sample was sent; "
                          f"each was placed on the next batch's first sample")
                backoff = 0.5
            except grpc.RpcError as err:
                if err.code() not in RETRYABLE:
                    raise ReplayError(f"gateway rejected the stream: {err.code().name}: {err.details()}") from err
                print(f"session {bridge.session_id}: disconnected ({err.code().name}); retrying in {backoff:.1f}s")
                time.sleep(backoff)
                backoff = min(backoff * 2, 10.0)
    except KeyboardInterrupt:
        stop.set()
    except ReplayError as err:
        raise SystemExit(str(err)) from err


if __name__ == "__main__":
    main()
