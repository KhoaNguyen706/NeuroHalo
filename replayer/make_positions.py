"""Write web/src/positions.json: where every standard sensor sits on the head.

Run once; the output is committed.

    uv run make_positions.py

Positions come from MNE's standard 10-05 layout. They are moved so the
best-fitting sphere through all sensors is centred on 0 with radius 1, and
turned into three.js axes: x to the subject's right, y up, z to the back of
the head (the nose points to -z).
"""

import json
from pathlib import Path

import mne
import numpy as np

OUT = Path(__file__).resolve().parent.parent / "web" / "src" / "positions.json"


def fit_sphere(points: np.ndarray) -> tuple[np.ndarray, float]:
    """Least-squares sphere through the points: returns (centre, radius)."""
    a = np.c_[2 * points, np.ones(len(points))]
    b = (points**2).sum(axis=1)
    sol, *_ = np.linalg.lstsq(a, b, rcond=None)
    centre = sol[:3]
    return centre, float(np.sqrt(sol[3] + centre @ centre))


def main() -> None:
    """Fit, normalise, rotate and write the positions."""
    montage = mne.channels.make_standard_montage("colin27_1005")
    ch_pos = montage.get_positions()["ch_pos"]
    names = list(ch_pos)
    pts = np.array([ch_pos[n] for n in names])
    centre, radius = fit_sphere(pts)
    unit = (pts - centre) / radius
    # MNE's frame: x right, y toward the nose, z up -> three.js (x, z, -y).
    out = {n: [round(float(x), 4), round(float(z), 4), round(float(-y), 4)] for n, (x, y, z) in zip(names, unit)}
    OUT.parent.mkdir(parents=True, exist_ok=True)
    OUT.write_text(json.dumps(out, separators=(",", ":")) + "\n")
    print(f"wrote {len(out)} positions to {OUT} (sphere radius {radius * 100:.1f} cm)")


if __name__ == "__main__":
    main()
