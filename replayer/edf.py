"""Load an EDF recording as microvolt float32 rows with standard channel names."""

from dataclasses import dataclass

import mne
import numpy as np


# One recording held in memory.
# Returned by: load_edf.
# Used by: inspect_edf.main, replay.parse_flatten, replay.Replay.
@dataclass
class Recording:
    sample_rate_hz: float
    channel_names: list[str]  # normalized, e.g. "Fp1", "Cz"
    raw_channel_names: list[str]  # as written in the file, e.g. "Fp1.", "Cz.."
    samples: np.ndarray  # shape (num_samples, num_channels), float32, microvolts


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
    """Read an EDF file and return it with normalized names, in microvolts."""
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
    )
