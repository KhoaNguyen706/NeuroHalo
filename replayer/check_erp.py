"""Check a session's ERPs against the same ERPs computed with MNE.

The processor keeps the raw signal around every marker (processor/epochs.go);
the api filters it, cuts out 0.2 s before to 0.8 s after the marker,
subtracts the baseline, leaves out trials over a peak-to-peak threshold per
sensor and averages per label (api/erp.go, GET /sessions/{id}/erp). This
script does the same from the original file with MNE only: MNE reads the
file, filters the whole recording with its default 30 Hz low-pass, and
mne.Epochs cuts and baselines the trials.

Compared, for every rejection threshold the page offers, label and sensor:
  - which trials count (the processor needs 2 s of signal before a marker and
    4 s after it; a sensor flat or over the threshold is left out per sensor)
  - the averaged voltage at every sample, in uV

    uv run check_erp.py SESSION_ID ../data/S001R03.edf

Only meaningful for a replay of the whole file. Exits with 1 when anything
does not match.
"""

import argparse
import json
import sys
import urllib.request
from pathlib import Path

import mne
import numpy as np

# The processor's and the api's constants (processor/epochs.go, api/erp.go).
BUFFER_SECONDS = 2.0
EPOCH_AFTER_SECONDS = 4.0
LOWPASS_HZ = 30.0
TMIN, TMAX = -0.2, 0.8
FLAT_UV = 1.0
REJECTS = [100, 200, 0]

# The api filters float32 samples in float64 and returns float32 means.
MAX_UV_ERROR = 1e-3


def fetch(api: str, token: str, path: str) -> dict:
    """GET one api path and return its JSON."""
    req = urllib.request.Request(f"{api}{path}", headers={"Authorization": f"Bearer {token}"})
    with urllib.request.urlopen(req) as res:
        return json.load(res)


def mne_epochs(raw: mne.io.BaseRaw) -> tuple[list[str], np.ndarray, np.ndarray]:
    """Filter the recording, cut and baseline every trial the processor keeps.
    Returns the labels, the times (s) and the data (trials x sensors x samples, uV)."""
    rate = raw.info["sfreq"]
    raw = raw.copy().filter(None, LOWPASS_HZ, verbose="error")  # MNE's defaults: FIR, zero phase
    events, ids = mne.events_from_annotations(raw, verbose="error")
    n = raw.n_times
    keep = [(s >= round(BUFFER_SECONDS * rate) - 1) and (s + round(EPOCH_AFTER_SECONDS * rate) <= n - 1)
            for s in events[:, 0]]
    events = events[np.array(keep)]
    epochs = mne.Epochs(raw, events, ids, tmin=TMIN, tmax=TMAX, baseline=(None, 0),
                        reject=None, flat=None, preload=True, verbose="error")
    names = {v: k for k, v in ids.items()}
    return [names[e] for e in epochs.events[:, 2]], epochs.times, epochs.get_data() * 1e6


def main() -> int:
    """Fetch, recompute, print one line per check, and return the exit code."""
    p = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    p.add_argument("session_id")
    p.add_argument("original", type=Path, help="the EDF file the session was replayed from")
    p.add_argument("--api", default="http://localhost:8080")
    p.add_argument("--token", default="dev-token")
    args = p.parse_args()

    raw = mne.io.read_raw_edf(args.original, preload=True, verbose="error")
    # Rounded to float32 first, as the replayer sends it.
    raw._data = raw.get_data().astype(np.float32).astype(np.float64)
    labels, times, data = mne_epochs(raw)
    span = np.ptp(data, axis=-1)                              # trials x sensors

    failures = []

    def check(ok: bool, text: str) -> None:
        """Print one check and remember a failure."""
        print(("ok    " if ok else "FAIL  ") + text)
        if not ok:
            failures.append(text)

    print(f"{args.original.name}: {len(labels)} trials, MNE {mne.__version__}")
    for reject in REJECTS:
        got = fetch(args.api, args.token, f"/sessions/{args.session_id}/erp?reject={reject}")
        name = f"reject {reject} uV" if reject else "keep all"
        check(len(got["times"]) == len(times) and np.allclose(got["times"], times, atol=1e-9),
              f"{name}: {len(got['times'])} samples from {got['times'][0]:+.3f} to {got['times'][-1]:+.3f} s")
        check(sorted(r["label"] for r in got["labels"]) == sorted(set(labels)),
              f"{name}: labels {', '.join(r['label'] for r in got['labels'])}")
        worst = 0.0
        for r in got["labels"]:
            pick = np.array([lab == r["label"] for lab in labels])
            good = (span[pick] >= FLAT_UV) & ((span[pick] <= reject) if reject else True)  # trials x sensors
            count = good.sum(axis=0)
            check(r["trials"] == int(pick.sum()) and list(r["good"]) == count.tolist(),
                  f"{name} {r['label']}: {int(pick.sum())} trials, {int(count.min())} to {int(count.max())} kept per sensor")
            total = (data[pick] * good[..., None]).sum(axis=0)  # sensors x samples
            mean = np.divide(total, count[:, None], out=np.zeros_like(total), where=count[:, None] > 0)
            ours = np.array(r["mean"], dtype=np.float64).reshape(len(times), -1).T
            worst = max(worst, float(np.abs(ours - mean).max()))
        check(worst < MAX_UV_ERROR, f"{name}: every averaged sample matches, worst {worst:.1e} uV off")

    print("all checks passed" if not failures else f"{len(failures)} checks failed")
    return 1 if failures else 0


if __name__ == "__main__":
    sys.exit(main())
