"""Write web/public/model/: the head, the brain and the sensor positions.

Run once; the output is committed.

    uv run make_head_model.py

Everything comes from MNE's standard anatomy, "fsaverage" (FreeSurfer's
average of 40 brains), which MNE downloads on first use:

  scalp   bem/fsaverage-head-medium.fif   the outer skin, about 30,000 triangles
  cortex  surf/lh.pial + rh.pial          the folded brain surface, thinned to
                                          about 164,000 triangles
  shade   surf/lh.sulc + rh.sulc          how deep each point sits in a fold,
                                          used to draw folds darker
  sensors MNE's standard 10-05 layout, moved onto this head and snapped to the skin

Output axes are three.js axes: x to the subject's right, y up, z to the back of
the head (the nose points to -z). One unit is 10 cm, and 0,0,0 is the middle
of the brain.

Files written (little-endian, no headers):
  scalp.pos.bin   float32 x,y,z per vertex      scalp.idx.bin   uint32 a,b,c per triangle
  cortex.pos.bin  float32 x,y,z per vertex      cortex.idx.bin  uint32 a,b,c per triangle
  cortex.shade.bin  uint8 per vertex, 0 = deep in a fold, 255 = top of a ridge
  electrodes.json   name -> {"p": [x,y,z], "n": [x,y,z]}  position and outward direction
"""

import json
from pathlib import Path

import mne
import nibabel.freesurfer as fsio
import numpy as np
from scipy.spatial import cKDTree

OUT = Path(__file__).resolve().parent.parent / "web" / "public" / "model"
UNITS_PER_METRE = 10.0
SENSOR_LIFT_M = 0.003  # sensors sit 3 mm above the skin


def vertex_normals(rr: np.ndarray, tris: np.ndarray) -> np.ndarray:
    """Unit normal per vertex: the area-weighted average of its triangles' normals."""
    face = np.cross(rr[tris[:, 1]] - rr[tris[:, 0]], rr[tris[:, 2]] - rr[tris[:, 0]])
    out = np.zeros_like(rr)
    for corner in range(3):
        np.add.at(out, tris[:, corner], face)
    return out / np.maximum(np.linalg.norm(out, axis=1, keepdims=True), 1e-12)


def load_cortex(subjects_dir: Path) -> tuple[np.ndarray, np.ndarray, np.ndarray]:
    """Both hemispheres of the pial surface, thinned to MNE's "ico6" spacing,
    as (vertices in metres, triangles, fold depth per vertex)."""
    src = mne.setup_source_space("fsaverage", spacing="ico6", surface="pial",
                                 subjects_dir=str(subjects_dir), add_dist=False, verbose="ERROR")
    all_rr, all_tris, all_sulc, offset = [], [], [], 0
    for hemi, s in zip(("lh", "rh"), src):
        used = s["vertno"]  # indexes into the full surface
        remap = np.full(len(s["rr"]), -1, dtype=np.int64)
        remap[used] = np.arange(len(used))
        sulc = fsio.read_morph_data(str(subjects_dir / "fsaverage" / "surf" / f"{hemi}.sulc"))
        all_rr.append(s["rr"][used])
        all_tris.append(remap[s["use_tris"]] + offset)
        all_sulc.append(sulc[used])
        offset += len(used)
    return np.vstack(all_rr), np.vstack(all_tris), np.concatenate(all_sulc)


def main() -> None:
    """Load the surfaces, place the sensors, convert axes and units, write the files."""
    fs = Path(mne.datasets.fetch_fsaverage(verbose="ERROR"))

    scalp = mne.read_bem_surfaces(str(fs / "bem" / "fsaverage-head-medium.fif"), on_defects="ignore", verbose="ERROR")[0]
    dense = mne.read_bem_surfaces(str(fs / "bem" / "fsaverage-head-dense.fif"), on_defects="ignore", verbose="ERROR")[0]
    cortex_rr, cortex_tris, sulc = load_cortex(fs.parent)

    # Sensors: standard layout (head coordinates) -> this head's MRI coordinates,
    # then onto the nearest point of the detailed skin surface.
    trans = mne.read_trans(str(fs / "bem" / "fsaverage-trans.fif"))  # head -> MRI
    montage = mne.channels.make_standard_montage("colin27_1005")
    names = list(montage.ch_names)
    # The layout's raw positions are in its own frame. Attaching it to an Info
    # converts them to head coordinates (defined by the nose bridge and ears),
    # which is the frame the transform starts from.
    info = mne.create_info(names, sfreq=1000.0, ch_types="eeg")
    info.set_montage(montage, verbose="ERROR")
    in_head = np.array([ch["loc"][:3] for ch in info["chs"]])
    in_mri = mne.transforms.apply_trans(trans["trans"], in_head)
    dense_normals = vertex_normals(dense["rr"], dense["tris"])
    gap, nearest = cKDTree(dense["rr"]).query(in_mri)
    sensor_n = dense_normals[nearest]
    sensor_p = dense["rr"][nearest] + sensor_n * SENSOR_LIFT_M
    print(f"sensors: {len(names)}, distance from layout position to skin: "
          f"median {np.median(gap) * 1000:.1f} mm, max {gap.max() * 1000:.1f} mm")

    centre = (cortex_rr.min(axis=0) + cortex_rr.max(axis=0)) / 2

    def to_scene(points: np.ndarray, is_direction: bool = False) -> np.ndarray:
        """MRI axes (x right, y front, z up, metres) -> three.js axes and units."""
        p = points if is_direction else (points - centre) * UNITS_PER_METRE
        return np.column_stack([p[:, 0], p[:, 2], -p[:, 1]])

    # Fold depth -> 0..255, light on ridges and dark in folds.
    shade = np.clip(0.5 - sulc / (2.5 * np.abs(sulc).std()), 0, 1)

    OUT.mkdir(parents=True, exist_ok=True)
    to_scene(scalp["rr"]).astype("<f4").tofile(OUT / "scalp.pos.bin")
    scalp["tris"].astype("<u4").tofile(OUT / "scalp.idx.bin")
    to_scene(cortex_rr).astype("<f4").tofile(OUT / "cortex.pos.bin")
    cortex_tris.astype("<u4").tofile(OUT / "cortex.idx.bin")
    (shade * 255).astype(np.uint8).tofile(OUT / "cortex.shade.bin")

    p, n = to_scene(sensor_p), to_scene(sensor_n, is_direction=True)
    electrodes = {
        name: {"p": [round(float(v), 4) for v in p[i]], "n": [round(float(v), 4) for v in n[i]]}
        for i, name in enumerate(names)
    }
    (OUT / "electrodes.json").write_text(json.dumps(electrodes, separators=(",", ":")) + "\n")

    for f in sorted(OUT.iterdir()):
        print(f"  {f.name:<18} {f.stat().st_size / 1024:8.0f} KB")
    print(f"scalp {len(scalp['rr'])} vertices / {len(scalp['tris'])} triangles, "
          f"cortex {len(cortex_rr)} vertices / {len(cortex_tris)} triangles")
    for name in ("Cz", "Fpz", "Oz", "T7", "T8"):
        print(f"  {name:<4} p={electrodes[name]['p']}")
    scene_scalp = to_scene(scalp["rr"])
    print("scalp extent in scene units: x", scene_scalp[:, 0].min().round(2), scene_scalp[:, 0].max().round(2),
          "y", scene_scalp[:, 1].min().round(2), scene_scalp[:, 1].max().round(2),
          "z", scene_scalp[:, 2].min().round(2), scene_scalp[:, 2].max().round(2))


if __name__ == "__main__":
    main()
