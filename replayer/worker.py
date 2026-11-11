"""Replay worker: plays the samples and uploads that the api queues.

The api inserts a row into the `replays` table; this worker claims it, fetches
the recording (an upload from S3 storage, or a sample from PhysioNet), and
streams it to the gateway like the command-line replayer does.

    uv run worker.py      # usually started by Docker Compose
"""

import logging
import math
import os
import re
import tempfile
import threading
import time
import urllib.request
from pathlib import Path

import psycopg
from minio import Minio

from edf import load_edf
from replay import ReplayError, apply_flatten, parse_speed, stream_recording

log = logging.getLogger("worker")

DATABASE_URL = os.environ.get("DATABASE_URL", "postgresql://neuro:neuro@localhost:5432/neuro")
GATEWAY = os.environ.get("GATEWAY", "localhost:50051")
INGEST_TOKEN = os.environ.get("INGEST_TOKEN", "dev-token")
MAX_JOBS = int(os.environ.get("MAX_JOBS", "8"))
SAMPLE_DIRS = [Path(p) for p in os.environ.get("SAMPLE_DIRS", "../data").split(os.pathsep) if p]
CACHE_DIR = Path(os.environ.get("CACHE_DIR", tempfile.gettempdir()) )

# Samples are fetched from PhysioNet by file name, never by a URL from the request.
SAMPLE_FILE = re.compile(r"^S(\d{3})R(\d{2})\.edf$")
PHYSIONET = "https://physionet.org/files/eegmmidb/1.0.0/S{subject}/{name}"

CLAIM_SQL = """
UPDATE replays SET status = 'running', updated_at = now()
WHERE session_id = (
    SELECT session_id FROM replays WHERE status = 'queued'
    ORDER BY created_at FOR UPDATE SKIP LOCKED LIMIT 1)
RETURNING session_id, kind, object_key, sample, speed, flatten, start_seconds, end_seconds
"""


def storage() -> Minio:
    """Return a client for the S3-compatible store that holds uploads."""
    return Minio(
        os.environ.get("S3_ENDPOINT", "localhost:8333"),
        access_key=os.environ.get("S3_ACCESS_KEY", "neuro"),
        secret_key=os.environ.get("S3_SECRET_KEY", "neuro-dev-secret"),
        secure=os.environ.get("S3_SECURE", "false") == "true",
    )


def finish(session_id: str, status: str, error: str = "") -> None:
    """Record how a job ended: done, or failed with a message for the web page."""
    with psycopg.connect(DATABASE_URL, autocommit=True) as conn:
        conn.execute(
            "UPDATE replays SET status = %s, error = %s, updated_at = now() WHERE session_id = %s",
            (status, error[:500], session_id),
        )


def sample_path(name: str) -> Path:
    """Find a sample recording on disk, downloading it into the cache the first
    time. Only names like S001R02.edf are accepted."""
    m = SAMPLE_FILE.match(name)
    if not m:
        raise ValueError(f"not a sample file name: {name!r}")
    for d in [*SAMPLE_DIRS, CACHE_DIR]:
        if (d / name).is_file():
            return d / name
    CACHE_DIR.mkdir(parents=True, exist_ok=True)
    target = CACHE_DIR / name
    partial = target.with_suffix(".part")
    url = PHYSIONET.format(subject=m.group(1), name=name)
    log.info("downloading sample %s", url)
    with urllib.request.urlopen(url, timeout=60) as resp, open(partial, "wb") as out:
        while chunk := resp.read(1 << 20):
            out.write(chunk)
    partial.replace(target)  # atomic, so a half-written file is never used
    return target


def run_job(job: dict) -> None:
    """Fetch one job's recording, replay it, and record the result. Runs in
    its own thread; any error marks the job failed instead of stopping the worker."""
    sid = job["session_id"]
    tmp: Path | None = None
    try:
        if job["kind"] == "upload":
            fd, name = tempfile.mkstemp(suffix=".edf")
            os.close(fd)
            tmp = Path(name)
            storage().fget_object(os.environ.get("S3_BUCKET", "recordings"), job["object_key"], str(tmp))
            path = tmp
        else:
            path = sample_path(job["sample"])

        rec = load_edf(str(path))
        if job["flatten"]:
            apply_flatten(job["flatten"], rec)
        speed = parse_speed(job["speed"])
        log.info("session %s: replaying %s at %s", sid, job["object_key"] or job["sample"],
                 "full speed" if math.isinf(speed) else f"{speed:g}x")
        stream_recording(rec, sid, GATEWAY, INGEST_TOKEN, speed, source=job["kind"], log=log.info,
                         start_seconds=job["start_seconds"], end_seconds=job["end_seconds"])
        finish(sid, "done")
    except (ReplayError, ValueError, OSError) as err:
        log.warning("session %s: failed: %s", sid, err)
        finish(sid, "failed", str(err))
    except Exception as err:  # MNE raises many types for unreadable files
        log.exception("session %s: failed", sid)
        finish(sid, "failed", f"could not read the recording: {err}")
    finally:
        if tmp is not None:
            tmp.unlink(missing_ok=True)


def claim() -> dict | None:
    """Take the oldest queued job, marking it running so no other worker takes it."""
    with psycopg.connect(DATABASE_URL, autocommit=True) as conn:
        row = conn.execute(CLAIM_SQL).fetchone()
    if row is None:
        return None
    return dict(zip(("session_id", "kind", "object_key", "sample", "speed", "flatten", "start_seconds", "end_seconds"), row))


def main() -> None:
    """Fail jobs left running by a previous worker, then claim and run jobs,
    at most MAX_JOBS at a time, forever."""
    logging.basicConfig(level=logging.INFO, format="%(asctime)s %(name)s %(message)s")
    while True:
        try:
            with psycopg.connect(DATABASE_URL, autocommit=True) as conn:
                n = conn.execute(
                    "UPDATE replays SET status = 'failed', error = 'worker restarted during the replay', "
                    "updated_at = now() WHERE status = 'running'").rowcount
            if n:
                log.info("marked %d interrupted replays as failed", n)
            break
        except psycopg.OperationalError as err:
            log.info("waiting for Postgres: %s", err)
            time.sleep(2)

    log.info("worker started: gateway %s, up to %d replays at once", GATEWAY, MAX_JOBS)
    active: list[threading.Thread] = []
    while True:
        active = [t for t in active if t.is_alive()]
        job = None
        if len(active) < MAX_JOBS:
            try:
                job = claim()
            except psycopg.OperationalError as err:
                log.warning("cannot reach Postgres: %s", err)
        if job is None:
            time.sleep(1)
            continue
        t = threading.Thread(target=run_job, args=(job,), name=job["session_id"], daemon=True)
        t.start()
        active.append(t)


if __name__ == "__main__":
    main()
