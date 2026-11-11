"""Replay one EDF recording to the gateway at real speed.

    uv run replay.py --file ../data/S001R02.edf
    uv run replay.py --file ../data/S001R02.edf --flatten Cz@20
    uv run replay.py --file ../data/S001R02.edf --speed fast
"""

import argparse
import math
import os
import threading
import time
import uuid
from collections.abc import Callable

import grpc

from edf import Recording, load_edf
from neuro.v1 import ingest_pb2, ingest_pb2_grpc

BATCH_SECONDS = 0.1
REWIND_BATCHES = 10
WARMUP_SECONDS = 2.0  # the processor needs a full 2-second window before its first frame
RETRYABLE = {grpc.StatusCode.UNAVAILABLE, grpc.StatusCode.DEADLINE_EXCEEDED, grpc.StatusCode.ABORTED}


# Raised when the gateway refuses a stream for a reason retrying cannot fix
# (wrong token, invalid data).
# Raised by: stream_recording.
# Caught by: main (exits with the message) and worker.run_job (marks the job failed).
class ReplayError(Exception):
    pass


def parse_speed(text: str) -> float:
    """Turn "1x", "2", "fast" etc. into a speed factor; "fast" means no waiting."""
    text = text.strip().lower()
    if text == "fast":
        return math.inf
    value = float(text.removesuffix("x"))
    if value <= 0:
        raise ValueError("speed must be positive")
    return value


def apply_flatten(spec: str, rec: Recording) -> None:
    """Apply CHANNEL@SECONDS: zero one sensor from that moment on.

    Changes rec.samples in place. Raises ValueError if the spec is wrong.
    """
    channel, _, seconds = spec.partition("@")
    lookup = {name.upper(): i for i, name in enumerate(rec.channel_names)}
    if channel.upper() not in lookup or not seconds:
        raise ValueError(f"flatten {spec!r}: want CHANNEL@SECONDS with a channel from the file")
    start = int(float(seconds) * rec.sample_rate_hz)
    rec.samples[start:, lookup[channel.upper()]] = 0.0


# One session being replayed: the recording plus how far it has been sent.
# Created by: stream_recording.
# Used by: stream_recording, which calls requests() once per connection and
# reads next_seq and done to decide whether to reconnect.
class Replay:
    def __init__(self, rec: Recording, session_id: str, speed: float, source: str,
                 stop: threading.Event | None = None, start_seconds: float = 0.0,
                 end_seconds: float = 0.0):
        """Work out the batch size and batch count for this recording, and the
        first batch to send. With start_seconds above 0 the replay begins 2
        seconds before that moment, so the first frame the processor emits is
        the one at start_seconds. seq and first_sample_index keep counting
        from the start of the recording, so times stay the recording's own.
        With end_seconds above 0 the replay stops at that moment instead of at
        the end of the recording."""
        self.rec = rec
        self.session_id = session_id
        self.speed = speed
        self.source = source
        self.stop = stop
        self.batch_samples = round(rec.sample_rate_hz * BATCH_SECONDS)
        self.num_batches = -(-len(rec.samples) // self.batch_samples)
        if end_seconds > 0:
            self.num_batches = min(self.num_batches, math.ceil(end_seconds / BATCH_SECONDS))
        self.first_seq = max(0, int((start_seconds - WARMUP_SECONDS) / BATCH_SECONDS)) if start_seconds > 0 else 0
        self.next_seq = self.first_seq  # survives reconnects

    @property
    def done(self) -> bool:
        """True once every batch has been handed to gRPC."""
        return self.next_seq >= self.num_batches

    def requests(self):
        """Yield the messages for one stream: the header, then every batch from
        next_seq on, each scheduled against a monotonic clock so timing does
        not drift. At infinite speed batches go out as fast as gRPC takes them.
        Stops early (ending the stream cleanly) when the stop event is set."""
        yield ingest_pb2.IngestRequest(
            header=ingest_pb2.SessionHeader(
                session_id=self.session_id,
                sample_rate_hz=self.rec.sample_rate_hz,
                channel_names=self.rec.channel_names,
                source=self.source,
            )
        )
        first_seq = self.next_seq
        t0 = time.monotonic()
        for seq in range(first_seq, self.num_batches):
            if self.stop is not None and self.stop.is_set():
                return
            due = t0 + (seq - first_seq) * BATCH_SECONDS / self.speed
            delay = due - time.monotonic()
            if delay > 0:
                time.sleep(delay)
            start = seq * self.batch_samples
            rows = self.rec.samples[start : start + self.batch_samples]
            yield ingest_pb2.IngestRequest(
                batch=ingest_pb2.SampleBatch(
                    session_id=self.session_id,
                    seq=seq,
                    first_sample_index=start,
                    sent_time_unix_nanos=time.time_ns(),
                    num_samples=len(rows),
                    values=rows.ravel(),
                )
            )
            self.next_seq = seq + 1


def stream_recording(rec: Recording, session_id: str, gateway: str, token: str,
                     speed: float = 1.0, source: str = "replayer",
                     stop: threading.Event | None = None,
                     log: Callable[[str], None] = print, start_seconds: float = 0.0,
                     end_seconds: float = 0.0) -> int:
    """Send a recording to the gateway (all of it, or the part from
    start_seconds to end_seconds; an end of 0 means the end of the recording),
    reconnecting with backoff until every batch is sent. Returns the number
    of batches in the recording. Raises ReplayError when the gateway refuses
    the stream for good, or when start_seconds is past the end."""
    replay = Replay(rec, session_id, speed, source, stop, start_seconds, end_seconds)
    if replay.first_seq >= replay.num_batches:
        raise ReplayError(f"cannot start at {start_seconds:g} s: the recording is shorter than that")
    log(f"session {session_id}: {replay.num_batches} batches of {replay.batch_samples} samples x "
        f"{len(rec.channel_names)} channels -> {gateway}")
    metadata = [("authorization", f"Bearer {token}")]
    backoff = 0.5
    while True:
        try:
            with grpc.insecure_channel(gateway) as channel:
                stub = ingest_pb2_grpc.IngestStub(channel)
                summary = stub.Stream(replay.requests(), metadata=metadata)
            if replay.done or (stop is not None and stop.is_set()):
                log(f"session {session_id}: done, gateway received {summary.batches_received} "
                    f"batches on the last stream, last seq {summary.last_seq}")
                return replay.num_batches
        except grpc.RpcError as err:
            if err.code() not in RETRYABLE:
                raise ReplayError(f"gateway rejected the stream: {err.code().name}: {err.details()}") from err
            # Batches in flight when the stream broke may be lost. Go back one second;
            # the processor drops the ones that did arrive, by seq.
            replay.next_seq = max(replay.first_seq, replay.next_seq - REWIND_BATCHES)
            log(f"session {session_id}: disconnected ({err.code().name}); "
                f"resuming from seq {replay.next_seq} in {backoff:.1f}s")
            time.sleep(backoff)
            backoff = min(backoff * 2, 10.0)
        else:
            backoff = 0.5


def main() -> None:
    """Parse the flags, load the file, and stream it to the gateway."""
    p = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    p.add_argument("--file", required=True, help="recording to replay (EDF)")
    p.add_argument("--gateway", default="localhost:50051", help="gateway address")
    p.add_argument("--session-id", default=None, help="defaults to a random ID")
    p.add_argument("--speed", default="1", help='1 is real time, 2 is double, "fast" is as fast as possible')
    p.add_argument("--flatten", metavar="CHANNEL@SECONDS", help="zero one sensor from that moment on")
    p.add_argument("--token", default=os.environ.get("INGEST_TOKEN", "dev-token"))
    args = p.parse_args()

    try:
        speed = parse_speed(args.speed)
        rec = load_edf(args.file)
        if args.flatten:
            apply_flatten(args.flatten, rec)
        stream_recording(rec, args.session_id or f"s-{uuid.uuid4().hex[:12]}", args.gateway, args.token, speed)
    except (ValueError, ReplayError) as err:
        raise SystemExit(str(err)) from err


if __name__ == "__main__":
    main()
