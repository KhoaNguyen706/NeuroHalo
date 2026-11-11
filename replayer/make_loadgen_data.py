"""Convert one EDF recording into the two files the Go load generator reads.

    uv run make_loadgen_data.py                       # data/S001R02.edf -> data/loadgen/
    uv run make_loadgen_data.py --file ../data/S001R01.edf

Go has no EDF reader worth adding, so the conversion happens once, here:
  recording.f32   every sample as a little-endian 32-bit float, in microvolts,
                  row by row (one row per sample, one value per channel)
  recording.json  {"sampleRateHz": 160, "channelNames": [...], "samples": 9760}
"""

import argparse
import json
from pathlib import Path

from edf import load_edf


def main() -> None:
    """Read the EDF file and write recording.f32 and recording.json."""
    here = Path(__file__).resolve().parent
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("--file", default=str(here.parent / "data" / "S001R02.edf"))
    parser.add_argument("--out", default=str(here.parent / "data" / "loadgen"))
    args = parser.parse_args()

    rec = load_edf(args.file)
    out = Path(args.out)
    out.mkdir(parents=True, exist_ok=True)
    rec.samples.astype("<f4").tofile(out / "recording.f32")
    (out / "recording.json").write_text(json.dumps({
        "sampleRateHz": rec.sample_rate_hz,
        "channelNames": rec.channel_names,
        "samples": int(rec.samples.shape[0]),
    }))
    print(f"wrote {out}: {rec.samples.shape[0]} samples x {rec.samples.shape[1]} channels at {rec.sample_rate_hz} Hz")


if __name__ == "__main__":
    main()
