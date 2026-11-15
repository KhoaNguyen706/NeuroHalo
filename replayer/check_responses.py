"""Check a session's averaged responses against the same averages computed with MNE.

The processor cuts the signal around every marker into 1-second windows and
measures each band's power in them (processor/epochs.go); the api averages
those per marker label (GET /sessions/{id}/responses). This script recomputes
all of that from the original file without any NeuroHalo code: MNE reads the
file and its markers, and MNE's Welch spectrum gives each window's power.

Compared, for every band, label, sensor and time step:
  - which trials count (an epoch needs 2 s of signal before its marker and
    4 s after it; a sensor flat or noisy anywhere in the epoch is left out)
  - the averaged power, which may differ from MNE's by one fixed scale factor
    only: NeuroHalo's power has no unit, MNE's is in uV^2/Hz
  - the change from before the marker, in percent, which is what the page
    shows and what a researcher reads; this one must match outright

    uv run check_responses.py SESSION_ID ../data/S001R03.edf

Only meaningful for a replay of the whole file without a flattened sensor.
Exits with 1 when anything does not match.
"""

import argparse
import json
import sys
import urllib.request
from pathlib import Path

import mne
import numpy as np
from mne.time_frequency import psd_array_welch

# The processor's constants (processor/epochs.go, internal/eeg/eeg.go).
WINDOW_SECONDS = 1.0
STEP_SECONDS = 0.25
PRE_STEPS = 5
STEPS = 21
BUFFER_SECONDS = 2.0
FLAT_STD_UV = 0.5
NOISY_PEAK_TO_PEAK_UV = 500.0
BANDS = [("delta", 1, 4), ("theta", 4, 8), ("alpha", 8, 13), ("beta", 13, 30), ("gamma", 30, 45)]

# float32 power summed over a few bins: a few parts in a million. Percent
# changes are printed to one decimal on the page.
MAX_RELATIVE_ERROR = 1e-4
MAX_PERCENT_POINTS = 0.01


def fetch(api: str, token: str, path: str) -> dict:
    """GET one api path and return its JSON."""
    req = urllib.request.Request(f"{api}{path}", headers={"Authorization": f"Bearer {token}"})
    with urllib.request.urlopen(req) as res:
        return json.load(res)


def band_mask(freqs: np.ndarray, band: int) -> np.ndarray:
    """The FFT bins of one band: low edge included, high edge excluded, except
    the last band, which includes its high edge (as internal/eeg does)."""
    _, lo, hi = BANDS[band]
    last = band == len(BANDS) - 1
    return (freqs >= lo) & ((freqs < hi) | (last & (freqs == hi)))


def compute_epochs(data: np.ndarray, rate: float, markers: list[tuple[int, str]]):
    """Recompute every epoch the processor would emit.

    data is sensors x samples in uV. Returns the labels of the kept epochs,
    their band power (epochs x steps x sensors x bands, MNE's unit) and which
    sensors were bad in each (epochs x sensors)."""
    win = round(WINDOW_SECONDS * rate)
    step = round(STEP_SECONDS * rate)
    buffer = round(BUFFER_SECONDS * rate)
    n = data.shape[1]
    kept = [(s, label) for s, label in markers
            if s >= buffer - 1 and s + (STEPS - PRE_STEPS) * step <= n - 1]

    # Window j of an epoch ends (j - (PRE_STEPS - 1)) steps after the marker,
    # and the marker's own sample is the last one of window PRE_STEPS - 1.
    ends = np.array([[s + (j - (PRE_STEPS - 1)) * step for j in range(STEPS)] for s, _ in kept])
    index = ends[..., None] - win + 1 + np.arange(win)        # epochs x steps x win
    windows = data[:, index].transpose(1, 2, 0, 3)          # epochs x steps x sensors x win

    psd, freqs = psd_array_welch(windows, rate, fmin=0, fmax=rate / 2, n_fft=win, n_per_seg=win,
                                 n_overlap=0, window="hann", average="mean", verbose="error")
    power = np.stack([psd[..., band_mask(freqs, b)].sum(-1) for b in range(len(BANDS))], axis=-1)

    std = windows.std(axis=-1)
    span = windows.max(axis=-1) - windows.min(axis=-1)
    bad = ((std < FLAT_STD_UV) | (span > NOISY_PEAK_TO_PEAK_UV)).any(axis=1)  # epochs x sensors
    return [label for _, label in kept], power, bad


def average(labels: list[str], power: np.ndarray, bad: np.ndarray, label: str, band: int):
    """Average one label's epochs in one band, sensor by sensor, leaving out the
    epochs where that sensor was bad. Returns (trials, good per sensor,
    steps x sensors mean power)."""
    pick = np.array([lab == label for lab in labels])
    good = ~bad[pick]                                       # trials x sensors
    p = power[pick][..., band]                              # trials x steps x sensors
    total = (p * good[:, None, :]).sum(axis=0)
    count = good.sum(axis=0)
    mean = np.divide(total, count, out=np.zeros_like(total), where=count > 0)
    return int(pick.sum()), count, mean


def change(mean: np.ndarray) -> np.ndarray:
    """Percent change of each step from the mean of the steps before the
    marker, per sensor, as the session page shows it."""
    before = mean[:PRE_STEPS].mean(axis=0)
    return np.divide(mean, before, out=np.full_like(mean, np.nan), where=before > 0) * 100 - 100


def main() -> int:
    """Fetch, recompute, print one line per check, and return the exit code."""
    p = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    p.add_argument("session_id")
    p.add_argument("original", type=Path, help="the EDF file the session was replayed from")
    p.add_argument("--api", default="http://localhost:8080")
    p.add_argument("--token", default="dev-token")
    args = p.parse_args()

    raw = mne.io.read_raw_edf(args.original, preload=True, verbose="error")
    rate = raw.info["sfreq"]
    # Rounded to float32 first, as the replayer sends it.
    data = (raw.get_data() * 1e6).astype(np.float32).astype(np.float64)
    markers = sorted((round(float(o) * rate), t) for o, t in zip(raw.annotations.onset, raw.annotations.description))
    labels, power, bad = compute_epochs(data, rate, markers)

    failures = []

    def check(ok: bool, text: str) -> None:
        """Print one check and remember a failure."""
        print(("ok    " if ok else "FAIL  ") + text)
        if not ok:
            failures.append(text)

    print(f"{args.original.name}: {len(markers)} markers, {len(labels)} complete epochs, MNE {mne.__version__}")
    for b, (name, _, _) in enumerate(BANDS):
        got = fetch(args.api, args.token, f"/sessions/{args.session_id}/responses?band={name}")
        sensors = len(raw.ch_names)
        check(got["preSteps"] == PRE_STEPS and len(got["times"]) == STEPS,
              f"{name}: {len(got['times'])} steps, {got['preSteps']} before the marker")
        check(sorted(r["label"] for r in got["labels"]) == sorted(set(labels)),
              f"{name}: labels {', '.join(r['label'] for r in got['labels'])}")
        ratios, worst_rel, worst_pct = [], 0.0, 0.0
        for r in got["labels"]:
            trials, good, mean = average(labels, power, bad, r["label"], b)
            check(r["trials"] == trials and list(r["good"]) == good.tolist(),
                  f"{name} {r['label']}: {trials} trials, {int(good.min())} to {int(good.max())} good per sensor")
            ours = np.array(r["power"], dtype=np.float64).reshape(STEPS, sensors)
            usable = (good > 0) & (mean > 0).all(axis=0)
            ratios.append((ours[:, usable], mean[:, usable]))
            worst_pct = max(worst_pct, float(np.nanmax(np.abs(change(ours[:, usable]) - change(mean[:, usable])))))
        scale = np.median(np.concatenate([(o / m).ravel() for o, m in ratios]))
        worst_rel = max(float(np.abs(o / (scale * m) - 1).max()) for o, m in ratios)
        check(worst_rel < MAX_RELATIVE_ERROR,
              f"{name}: power = {scale:.6g} x MNE's in every cell, worst {worst_rel:.1e} off")
        check(worst_pct < MAX_PERCENT_POINTS,
              f"{name}: change from before the marker matches, worst {worst_pct:.4f} percentage points off")

    print("all checks passed" if not failures else f"{len(failures)} checks failed")
    return 1 if failures else 0


if __name__ == "__main__":
    sys.exit(main())
