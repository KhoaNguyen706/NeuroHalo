"""Build step 1: print what is inside an EDF file.

    uv run inspect_edf.py ../data/S001R01.edf
"""

import sys

import numpy as np

from edf import _standard_names, load_edf


def main() -> None:
    """Print the sample rate, size, channel names and first rows of one file."""
    if len(sys.argv) != 2:
        sys.exit("usage: inspect_edf.py FILE.edf")
    rec = load_edf(sys.argv[1])
    n_samples, n_channels = rec.samples.shape

    print(f"file:         {sys.argv[1]}")
    print(f"sample rate:  {rec.sample_rate_hz} Hz")
    print(f"channels:     {n_channels}")
    print(f"samples:      {n_samples} ({n_samples / rec.sample_rate_hz:.1f} s)")
    print(f"value range:  {rec.samples.min():.1f} to {rec.samples.max():.1f} uV")

    print("\nchannel names (file -> normalized):")
    pairs = [f"{raw!r:>8} -> {name:<4}" for raw, name in zip(rec.raw_channel_names, rec.channel_names)]
    for i in range(0, len(pairs), 4):
        print("  " + "   ".join(pairs[i : i + 4]))

    known = set(_standard_names().values())
    unknown = [n for n in rec.channel_names if n not in known]
    print(f"\nnames without a standard_1005 position: {unknown or 'none'}")

    print("\nfirst 5 rows, first 8 channels (uV):")
    print("  " + " ".join(f"{n:>7}" for n in rec.channel_names[:8]))
    with np.printoptions(precision=1, suppress=True):
        for row in rec.samples[:5, :8]:
            print("  " + " ".join(f"{v:7.1f}" for v in row))


if __name__ == "__main__":
    main()
