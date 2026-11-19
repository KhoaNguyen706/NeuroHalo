"""Load an EDF recording as microvolt float32 rows with standard channel names."""

from dataclasses import dataclass

import mne
import numpy as np


MAX_LABEL_BYTES = 64  # the gateway refuses longer marker labels


# One thing that happened in the experiment (a cue shown, a task begun), as
# the file's annotations record it.
# Built by: load_edf. Used by: replay.Replay, lsl_sim.main.
@dataclass
class Marker:
    sample_index: int  # position in the recording
    label: str  # as written in the file, e.g. "T1"
    duration_seconds: float  # 0 for a single moment


# One recording held in memory.
# Returned by: load_edf.
# Used by: inspect_edf.main, replay.parse_flatten, replay.Replay.
@dataclass
class Recording:
    sample_rate_hz: float
    channel_names: list[str]  # normalized, e.g. "Fp1", "Cz"
    raw_channel_names: list[str]  # as written in the file, e.g. "Fp1.", "Cz.."
    samples: np.ndarray  # shape (num_samples, num_channels), float32, microvolts
    markers: list[Marker]  # in time order; empty when the file has no annotations


def clip_label(text: str) -> str:
    """Cut a marker label down to MAX_LABEL_BYTES bytes of UTF-8, never in the
    middle of a character."""
    return text.strip().encode()[:MAX_LABEL_BYTES].decode(errors="ignore")


def read_markers(raw: "mne.io.BaseRaw") -> list[Marker]:
    """Turn the file's annotations into markers, in time order. Annotations
    with an empty text, or outside the recording, are left out."""
    rate = float(raw.info["sfreq"])
    markers = []
    for onset, duration, text in zip(raw.annotations.onset, raw.annotations.duration, raw.annotations.description):
        index = round(float(onset) * rate)
        label = clip_label(str(text))
        if label and 0 <= index < raw.n_times:
            markers.append(Marker(index, label, max(0.0, float(duration))))
    return sorted(markers, key=lambda m: m.sample_index)


def _standard_names() -> dict[str, str]:
    """Map upper-cased name -> standard 10-05 spelling ("FCZ" -> "FCz")."""
    # MNE 1.13 renamed "standard_1005" to "colin27_1005"; same layout.
    montage = mne.channels.make_standard_montage("colin27_1005")
    return {name.upper(): name for name in montage.ch_names}


def normalize_channel_name(name: str, standard: dict[str, str]) -> str:
    """Turn a name from the file into its standard spelling ("Fc5." -> "FC5").

    The dataset pads names with dots and uses odd casing. A name with no
    standard spelling is returned with only the dots removed.
    """
    stripped = name.strip().strip(".")
    return standard.get(stripped.upper(), stripped)


def load_edf(path: str) -> Recording:
    """Read an EDF file and return it with normalized names, in microvolts,
    together with the markers from its annotations."""
    raw = mne.io.read_raw_edf(path, preload=True, verbose="ERROR")
    standard = _standard_names()
    raw_names = list(raw.ch_names)
    names = [normalize_channel_name(n, standard) for n in raw_names]
    # MNE returns volts as (channels, samples); we want microvolts, one row per sample.
    samples = (raw.get_data() * 1e6).T.astype(np.float32)
    return Recording(
        sample_rate_hz=float(raw.info["sfreq"]),
        channel_names=names,
        raw_channel_names=raw_names,
        samples=np.ascontiguousarray(samples),
        markers=read_markers(raw),
    )
