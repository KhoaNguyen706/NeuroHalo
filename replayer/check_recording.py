"""Check a session's recorded EDF file against the file it was replayed from.

Downloads GET /sessions/{id}/recording.edf, reads it with MNE (a reader
NeuroHalo did not write), and compares it with the original recording as the
replayer sent it: sensor names, sample rate, every sample, and every marker.
The recorder stores 16 bits per sample, scaled to each sensor's own range, so
a sample may differ by at most half of one step of that scale.

    uv run check_recording.py SESSION_ID ../data/S001R03.edf

Only meaningful for a replay of the whole file without a flattened sensor.
Exits with 1 when anything does not match.
"""

import argparse
import sys
import tempfile
import urllib.request
from pathlib import Path

import mne
import numpy as np

from edf import load_edf

NO_DATA = "NeuroHalo: no data"  # the recorder's annotation where it had no samples


def download(api: str, token: str, session_id: str, target: Path) -> None:
    """Save the session's EDF file to target."""
    req = urllib.request.Request(f"{api}/sessions/{session_id}/recording.edf",
                                 headers={"Authorization": f"Bearer {token}"})
    with urllib.request.urlopen(req) as res, open(target, "wb") as out:
        out.write(res.read())


def main() -> int:
    """Download, compare, print one line per check, and return the exit code."""
    p = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    p.add_argument("session_id")
    p.add_argument("original", type=Path, help="the EDF file the session was replayed from")
    p.add_argument("--api", default="http://localhost:8080")
    p.add_argument("--token", default="dev-token")
    args = p.parse_args()

    sent = load_edf(args.original)
    with tempfile.TemporaryDirectory() as tmp:
        path = Path(tmp) / f"{args.session_id}.edf"
        download(args.api, args.token, args.session_id, path)
        size = path.stat().st_size
        raw = mne.io.read_raw_edf(path, preload=True, verbose="error")
    got = raw.get_data().T * 1e6  # MNE returns volts; rows of samples, in uV
    n = len(sent.samples)
    failures = []

    def check(ok: bool, text: str) -> None:
        """Print one check and remember a failure."""
        print(("ok    " if ok else "FAIL  ") + text)
        if not ok:
            failures.append(text)

    print(f"{path.name}: {size:,} bytes, read by MNE {mne.__version__}")
    check(raw.ch_names == sent.channel_names, f"{len(raw.ch_names)} sensor names match")
    check(raw.info["sfreq"] == sent.sample_rate_hz, f"sample rate {raw.info['sfreq']:g} Hz")
    check(len(got) >= n and len(got) - n < sent.sample_rate_hz,
          f"{n} samples per sensor ({len(got) - n} padding samples to fill the last 1-second record)")

    # The recorder scales each sensor's own range onto 65,536 steps; its header
    # range is rounded outwards to 0.001 uV, hence the small margin.
    span = sent.samples.max(axis=0) - sent.samples.min(axis=0)
    half_step = (span + 0.002) / 65535 / 2 + 1e-6
    error = np.abs(got[:n] - sent.samples).max(axis=0)
    worst = int(np.argmax(error / half_step))
    check(bool(np.all(error <= half_step)),
          f"every sample within half a step: worst sensor {sent.channel_names[worst]}, "
          f"{error[worst]:.4f} uV off, step {2 * half_step[worst]:.4f} uV")
    check(bool(np.all(got[n:] == 0)), "padding is 0 uV")

    notes = [(o, d, t) for o, d, t in zip(raw.annotations.onset, raw.annotations.duration, raw.annotations.description)]
    markers = [(o, d, t) for o, d, t in notes if t != NO_DATA]
    gaps = [(o, d) for o, d, t in notes if t == NO_DATA and o < n / sent.sample_rate_hz]
    check(len(markers) == len(sent.markers), f"{len(markers)} markers ({', '.join(sorted({t for _, _, t in markers}))})")
    mismatched = [
        (m, (o, d, t)) for m, (o, d, t) in zip(sent.markers, markers)
        if t != m.label or abs(o - m.sample_index / sent.sample_rate_hz) > 1e-6 or abs(d - m.duration_seconds) > 1e-6
    ]
    check(not mismatched, "every marker at its sample, with its label and duration"
          + (f"; first mismatch {mismatched[0]}" if mismatched else ""))
    check(not gaps, "no stretch of missing samples" + (f"; gaps at {gaps}" if gaps else ""))

    print("all checks passed" if not failures else f"{len(failures)} checks failed")
    return 1 if failures else 0


if __name__ == "__main__":
    sys.exit(main())
