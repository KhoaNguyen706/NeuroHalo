"""Publish a recording as a live LSL stream, standing in for an amplifier.

    uv run --group lsl lsl_sim.py                         # data/S001R01.edf, over and over
    uv run --group lsl lsl_sim.py --file ../data/S001R02.edf --seconds 60

Then, in another terminal:  uv run --group lsl lsl_bridge.py
"""

import argparse
import time
from pathlib import Path

import pylsl

from edf import load_edf

CHUNK_SECONDS = 0.05  # amplifiers send small chunks; 50 ms is typical


def main() -> None:
    """Load the file, announce the stream, and push samples at real speed
    until --seconds have passed or Ctrl+C. The recording starts over when it
    runs out."""
    here = Path(__file__).resolve().parent
    p = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    p.add_argument("--file", default=str(here.parent / "data" / "S001R01.edf"))
    p.add_argument("--name", default="NeuroHalo-sim")
    p.add_argument("--seconds", type=float, default=0, help="stop after this long; 0 runs until Ctrl+C")
    args = p.parse_args()

    rec = load_edf(args.file)
    info = pylsl.StreamInfo(args.name, "EEG", len(rec.channel_names), rec.sample_rate_hz, "float32", "neurohalo-sim")
    channels = info.desc().append_child("channels")
    for name in rec.channel_names:
        channel = channels.append_child("channel")
        channel.append_child_value("label", name)
        channel.append_child_value("unit", "microvolts")
        channel.append_child_value("type", "EEG")
    outlet = pylsl.StreamOutlet(info)
    print(f"LSL stream {args.name!r}: {len(rec.channel_names)} channels at {rec.sample_rate_hz:g} Hz, from {args.file}")

    chunk = round(rec.sample_rate_hz * CHUNK_SECONDS)
    t0 = time.monotonic()
    n = 0
    try:
        while args.seconds <= 0 or n * CHUNK_SECONDS < args.seconds:
            delay = t0 + n * CHUNK_SECONDS - time.monotonic()
            if delay > 0:
                time.sleep(delay)
            start = (n * chunk) % (len(rec.samples) - chunk)
            outlet.push_chunk(rec.samples[start : start + chunk].tolist())
            n += 1
    except KeyboardInterrupt:
        pass


if __name__ == "__main__":
    main()
