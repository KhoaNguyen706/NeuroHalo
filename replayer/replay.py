"""Replay one EDF recording to the gateway at real speed.

    uv run replay.py --file ../data/S001R02.edf
    uv run replay.py --file ../data/S001R02.edf --flatten Cz@20
"""

import argparse
import os
import time
import uuid

import grpc

from edf import Recording, load_edf
from neuro.v1 import ingest_pb2, ingest_pb2_grpc

BATCH_SECONDS = 0.1
REWIND_BATCHES = 10
RETRYABLE = {grpc.StatusCode.UNAVAILABLE, grpc.StatusCode.DEADLINE_EXCEEDED, grpc.StatusCode.ABORTED}


def parse_flatten(spec: str, rec: Recording) -> None:
    """Apply --flatten CHANNEL@SECONDS: zero one sensor from that moment on.

    Changes rec.samples in place. Exits with a message if the spec is wrong.
    """
    channel, _, seconds = spec.partition("@")
    lookup = {name.upper(): i for i, name in enumerate(rec.channel_names)}
    if channel.upper() not in lookup or not seconds:
        raise SystemExit(f"--flatten {spec!r}: want CHANNEL@SECONDS with a channel from the file")
    start = int(float(seconds) * rec.sample_rate_hz)
    rec.samples[start:, lookup[channel.upper()]] = 0.0


# One session being replayed: the recording plus how far it has been sent.
# Created by: main.
# Used by: main, which calls requests() once per connection and reads
# next_seq and done to decide whether to reconnect.
class Replay:
    def __init__(self, rec: Recording, session_id: str, speed: float):
        """Work out the batch size and batch count for this recording."""
        self.rec = rec
        self.session_id = session_id
        self.speed = speed
        self.batch_samples = round(rec.sample_rate_hz * BATCH_SECONDS)
        self.num_batches = -(-len(rec.samples) // self.batch_samples)
        self.next_seq = 0  # survives reconnects

    @property
    def done(self) -> bool:
        """True once every batch has been handed to gRPC."""
        return self.next_seq >= self.num_batches

    def requests(self):
        """Yield the messages for one stream: the header, then every batch from
        next_seq on, each scheduled against a monotonic clock so timing does
        not drift."""
        yield ingest_pb2.IngestRequest(
            header=ingest_pb2.SessionHeader(
                session_id=self.session_id,
                sample_rate_hz=self.rec.sample_rate_hz,
                channel_names=self.rec.channel_names,
                source="replayer",
            )
        )
        first_seq = self.next_seq
        t0 = time.monotonic()
        for seq in range(first_seq, self.num_batches):
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


def main() -> None:
    """Parse the flags, load the file, and stream it to the gateway,
    reconnecting with backoff until every batch is sent."""
    p = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    p.add_argument("--file", required=True, help="recording to replay (EDF)")
    p.add_argument("--gateway", default="localhost:50051", help="gateway address")
    p.add_argument("--session-id", default=None, help="defaults to a random ID")
    p.add_argument("--speed", type=float, default=1.0, help="1.0 is real time")
    p.add_argument("--flatten", metavar="CHANNEL@SECONDS", help="zero one sensor from that moment on")
    p.add_argument("--token", default=os.environ.get("INGEST_TOKEN", "dev-token"))
    args = p.parse_args()
    if args.speed <= 0:
        raise SystemExit("--speed must be positive")

    rec = load_edf(args.file)
    if args.flatten:
        parse_flatten(args.flatten, rec)
    replay = Replay(rec, args.session_id or f"s-{uuid.uuid4().hex[:12]}", args.speed)
    print(f"session {replay.session_id}: {replay.num_batches} batches of "
          f"{replay.batch_samples} samples x {len(rec.channel_names)} channels -> {args.gateway}")

    metadata = [("authorization", f"Bearer {args.token}")]
    backoff = 0.5
    while True:
        try:
            with grpc.insecure_channel(args.gateway) as channel:
                stub = ingest_pb2_grpc.IngestStub(channel)
                summary = stub.Stream(replay.requests(), metadata=metadata)
            if replay.done:
                print(f"done: gateway received {summary.batches_received} batches on the "
                      f"last stream, last seq {summary.last_seq}")
                return
        except grpc.RpcError as err:
            if err.code() not in RETRYABLE:
                raise SystemExit(f"gateway rejected the stream: {err.code().name}: {err.details()}")
            # Batches in flight when the stream broke may be lost. Go back one second;
            # the processor drops the ones that did arrive, by seq.
            replay.next_seq = max(0, replay.next_seq - REWIND_BATCHES)
            print(f"disconnected ({err.code().name}); resuming from seq {replay.next_seq} in {backoff:.1f}s")
            time.sleep(backoff)
            backoff = min(backoff * 2, 10.0)
        else:
            backoff = 0.5


if __name__ == "__main__":
    main()
