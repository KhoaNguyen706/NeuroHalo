"""Check a session's ERPs against the same ERPs computed with ERPLAB.

ERPLAB (Lopez-Calderon & Luck, 2014) is the EEGLAB toolbox many ERP labs
average with, and where the SME comes from. It runs in MATLAB; here it runs
in GNU Octave in Docker (the gnuoctave/octave image), on EEGLAB's and
ERPLAB's own source, so nothing has to be installed.

ERPLAB is handed the recording already low-passed the way NeuroHalo does it
(MNE's default 30 Hz FIR, which check_erp.py already compares tap for tap),
so this check covers the steps after the filter: cutting the trials, the
baseline, leaving out trials per sensor, the average, and the SME in
ERPLAB's default windows (replayer/erplab_erp.m does those steps with
ERPLAB's functions).

Compared, for every marker label and sensor, with every trial kept and with
trials over a peak-to-peak threshold left out per sensor:
  - how many trials are in the average
  - the averaged voltage at every sample, in uV
  - the aSME of every 100 ms window, in uV

    uv run check_erplab.py SESSION_ID ../data/S001R03.edf --eeglab DIR --erplab DIR

DIR are the unpacked EEGLAB (github.com/sccn/eeglab) and ERPLAB
(github.com/ucdavis/erplab) releases. Only meaningful for a replay of the
whole file. Exits with 1 when anything does not match.
"""

import argparse
import json
import subprocess
import sys
import tempfile
import urllib.request
from pathlib import Path

import mne
import numpy as np
import scipy.io

# The processor's and the api's constants (processor/epochs.go, api/erp.go).
BUFFER_SECONDS = 2.0
EPOCH_AFTER_SECONDS = 4.0
LOWPASS_HZ = 30.0
FLAT_UV = 1.0
OCTAVE_IMAGE = "gnuoctave/octave:9.2.0"

# ERPLAB averages in float64, the api in float64 from float32 samples.
MAX_UV_ERROR = 1e-3


def fetch(api: str, token: str, path: str) -> dict:
    """GET one api path and return its JSON."""
    req = urllib.request.Request(f"{api}{path}", headers={"Authorization": f"Bearer {token}"})
    with urllib.request.urlopen(req) as res:
        return json.load(res)


def prepare(edf: Path, workdir: Path, reject: float) -> list[str]:
    """Filter the recording as NeuroHalo does, keep the markers the processor
    keeps, and write them to input.mat for erplab_erp.m. Returns the labels,
    in code order (code 1 is the first)."""
    raw = mne.io.read_raw_edf(edf, preload=True, verbose="error")
    raw._data = raw.get_data().astype(np.float32).astype(np.float64)  # as the replayer sends it
    raw.filter(None, LOWPASS_HZ, verbose="error")
    events, ids = mne.events_from_annotations(raw, verbose="error")
    rate, n = raw.info["sfreq"], raw.n_times
    keep = [(s >= round(BUFFER_SECONDS * rate) - 1) and (s + round(EPOCH_AFTER_SECONDS * rate) <= n - 1)
            for s in events[:, 0]]
    events = events[np.array(keep)]
    names = {v: k for k, v in ids.items()}
    labels = sorted({names[e] for e in events[:, 2]})
    scipy.io.savemat(workdir / "input.mat", {
        "data": raw.get_data() * 1e6,
        "srate": rate,
        "channels": np.array(raw.ch_names, dtype=object),
        "labels": np.array(labels, dtype=object),
        "codes": np.array([labels.index(names[e]) + 1 for e in events[:, 2]], dtype=float),
        "latencies": events[:, 0].astype(float) + 1,  # EEGLAB counts samples from 1
        "reject": float(reject),
    })
    return labels


def run_erplab(workdir: Path, eeglab: Path, erplab: Path) -> None:
    """Run erplab_erp.m in Octave in Docker, with EEGLAB and ERPLAB on the path."""
    here = Path(__file__).resolve().parent
    # Two functions are replaced by empty ones; neither touches the data:
    #  - dipfitdefs, which EEGLAB's eeg_checkset calls, is in the DIPFIT
    #    plugin that the release zip leaves out (a git submodule); it only
    #    upgrades old head models, which these recordings do not have.
    #  - ERPLAB's erphistory keeps the history of commands run; one of its
    #    regular expressions is valid in MATLAB but not in Octave.
    # and ERPLAB's binlister asks Java for the user's home folder, to note
    # who made the event list; the Octave image has no Java, so it gets /root.
    stubs = workdir / "stubs"
    stubs.mkdir(exist_ok=True)
    (stubs / "dipfitdefs.m").write_text("function dipfitdefs()\nend\n")
    (stubs / "erphistory.m").write_text(
        "function [ERPLAB, ALLERPCOM] = erphistory(ERPLAB, ALLERPCOM, erpcom, psource)\nend\n")
    (stubs / "java.m").write_text(
        "function j = java()\n  j.lang.System.getProperty = @(name) '/root';\nend\n")
    # ERPLAB's ismember_bc2 asks MATLAB's ismember for its 'legacy' behaviour,
    # which Octave's does not take: the same answer, with the index of the
    # last match in B, as MATLAB's legacy mode gives it.
    (stubs / "ismember_bc2.m").write_text(
        "function [C, IA] = ismember_bc2(A, B, varargin)\n"
        "  varargin(strcmp(varargin, 'legacy')) = [];\n"
        "  if ~isempty(varargin)\n"
        "    [C, IA] = ismember(A, B, varargin{:});\n"
        "    return\n"
        "  end\n"
        "  B = B(:);\n"
        "  [C, IA] = ismember(A, B(end:-1:1));\n"
        "  IA(C) = numel(B) - IA(C) + 1;\n"
        "end\n")
    code = (
        "warning('off', 'all'); "
        "addpath(genpath('/eeglab/functions')); addpath(genpath('/erplab')); addpath('/check'); "
        "addpath('/work/stubs'); erplab_erp('/work');")
    subprocess.run([
        "docker", "run", "--rm",
        "-v", f"{eeglab}:/eeglab:ro", "-v", f"{erplab}:/erplab:ro", "-v", f"{here}:/check:ro", "-v", f"{workdir}:/work",
        OCTAVE_IMAGE, "octave-cli", "--no-gui", "--eval", code,
    ], check=True)


def main() -> int:
    """Prepare, run ERPLAB, fetch, print one line per check, and return the exit code."""
    p = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    p.add_argument("session_id")
    p.add_argument("original", type=Path, help="the EDF file the session was replayed from")
    p.add_argument("--eeglab", type=Path, required=True, help="the unpacked EEGLAB release")
    p.add_argument("--erplab", type=Path, required=True, help="the unpacked ERPLAB release")
    p.add_argument("--reject", type=float, default=200, help="the per-sensor threshold to compare, in uV")
    p.add_argument("--api", default="http://localhost:8080")
    p.add_argument("--token", default="dev-token")
    args = p.parse_args()

    failures = []

    def check(ok: bool, text: str) -> None:
        """Print one check and remember a failure."""
        print(("ok    " if ok else "FAIL  ") + text)
        if not ok:
            failures.append(text)

    with tempfile.TemporaryDirectory() as tmp:
        work = Path(tmp)
        labels = prepare(args.original, work, args.reject)
        run_erplab(work, args.eeglab.resolve(), args.erplab.resolve())
        out = scipy.io.loadmat(work / "output.mat", squeeze_me=False, struct_as_record=False)

    times = out["times"].ravel() / 1000
    windows = out["windows"]
    print(f"{args.original.name}: labels {', '.join(labels)}; ERPLAB's {len(windows)} aSME windows "
          f"from {windows[0, 0]:.0f} to {windows[-1, 1]:.0f} ms")

    for reject, mine in [(0, out["keep_all"][0, 0]), (args.reject, out["reject"][0, 0])]:
        name = f"reject {reject:g} uV" if reject else "keep all"
        got = fetch(args.api, args.token, f"/sessions/{args.session_id}/erp?reject={reject:g}")
        same_times = len(got["times"]) == len(times) and np.allclose(got["times"], times, atol=1e-6)
        check(same_times, f"{name}: {len(times)} samples from {times[0]:+.3f} to {times[-1]:+.3f} s")
        same_windows = len(got["bins"]) == len(windows) and np.allclose(got["bins"], windows[:, 0] / 1000, atol=1e-6)
        check(same_windows, f"{name}: the same {len(windows)} SME windows")
        if not (same_times and same_windows):
            continue  # nothing else can be compared sample by sample
        mean, sme = mine.mean, mine.sme                  # sensors x samples x bins, sensors x windows x bins
        accepted = np.atleast_2d(mine.accepted)          # bins (keep all) or sensors x bins (per sensor)
        worst = worst_sme = 0.0
        for r in got["labels"]:
            b = labels.index(r["label"])
            good = np.array(r["good"])
            # ERPLAB keeps flat trials; NeuroHalo leaves out trials under 1 uV, which this file has none of.
            want = accepted[0, b] * np.ones_like(good) if accepted.shape[0] == 1 else accepted[:, b]
            check(np.array_equal(good, want),
                  f"{name} {r['label']}: trials kept per sensor match ({int(good.min())} to {int(good.max())})")
            ours = np.array(r["mean"], dtype=np.float64).reshape(len(times), -1).T
            theirs = mean[:, :, b]
            use = good > 0
            worst = max(worst, float(np.abs(ours[use] - theirs[use]).max(initial=0)))
            ours_sme = np.array(r["sme"], dtype=np.float64).reshape(len(windows), -1).T
            use = good >= 2
            worst_sme = max(worst_sme, float(np.abs(ours_sme[use] - sme[use, :, b]).max(initial=0)))
        check(worst < MAX_UV_ERROR, f"{name}: every averaged sample matches ERPLAB, worst {worst:.1e} uV off")
        check(worst_sme < MAX_UV_ERROR, f"{name}: every window's SME matches ERPLAB's aSME, worst {worst_sme:.1e} uV off")

    print("all checks passed" if not failures else f"{len(failures)} checks failed")
    return 1 if failures else 0


if __name__ == "__main__":
    sys.exit(main())
