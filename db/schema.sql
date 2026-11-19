-- Postgres holds sessions, events, 10-second rollups, epochs and summaries.
-- Raw samples are not stored here: the recorder writes them to the object
-- store (table recordings).

CREATE TABLE IF NOT EXISTS sessions (
  id             text PRIMARY KEY,
  source         text NOT NULL,
  sample_rate_hz double precision NOT NULL,
  channel_names  text[] NOT NULL,
  started_at     timestamptz NOT NULL,
  ended_at       timestamptz,
  status         text NOT NULL DEFAULT 'live',  -- live | ended
  last_seq       bigint                         -- the last batch stored, set when the stream closes cleanly
);
-- For databases created before last_seq existed.
ALTER TABLE sessions ADD COLUMN IF NOT EXISTS last_seq bigint;

-- A study groups sessions for export as one BIDS dataset. Inside a study each
-- session is tagged with who was recorded (participant) and what they did
-- (task); both become part of the file names, so they are letters and digits
-- only, as BIDS requires. Several sessions of one participant and task
-- become runs 1, 2, ... in the order they started.
CREATE TABLE IF NOT EXISTS studies (
  id         text PRIMARY KEY,
  name       text NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now()
);
ALTER TABLE sessions ADD COLUMN IF NOT EXISTS study_id text REFERENCES studies(id);
ALTER TABLE sessions ADD COLUMN IF NOT EXISTS participant text NOT NULL DEFAULT '';
ALTER TABLE sessions ADD COLUMN IF NOT EXISTS task text NOT NULL DEFAULT '';
CREATE INDEX IF NOT EXISTS sessions_study ON sessions (study_id) WHERE study_id IS NOT NULL;

-- The raw signal of each session, kept by the recorder. While the session
-- runs, its batches go to the object store as chunks (spool/<id>/...); once it
-- has ended they are merged into one EDF+ file (recordings/<id>.edf).
CREATE TABLE IF NOT EXISTS recordings (
  session_id text PRIMARY KEY REFERENCES sessions(id),
  status     text NOT NULL DEFAULT 'recording', -- recording | writing | ready | failed
  saved_seq  bigint NOT NULL,                   -- the highest batch seq stored in a chunk
  object_key text NOT NULL DEFAULT '',          -- the EDF file, once ready
  bytes      bigint NOT NULL DEFAULT 0,         -- its size
  seconds    double precision NOT NULL DEFAULT 0, -- the signal it holds
  error      text NOT NULL DEFAULT '',
  updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS events (
  id         bigserial PRIMARY KEY,
  session_id text NOT NULL REFERENCES sessions(id),
  type       text NOT NULL,
  channel    text NOT NULL DEFAULT '',
  at         timestamptz NOT NULL,
  details    jsonb NOT NULL DEFAULT '{}',
  UNIQUE (session_id, type, channel, at)        -- makes replays of the same event harmless
);
CREATE INDEX IF NOT EXISTS events_session_at ON events (session_id, at);

CREATE TABLE IF NOT EXISTS rollups (
  session_id   text NOT NULL REFERENCES sessions(id),
  bucket_start timestamptz NOT NULL,            -- 10-second buckets
  band_power   real[] NOT NULL,                 -- channels x 5 bands, averaged over the bucket
  bad_channels text[] NOT NULL DEFAULT '{}',    -- not OK in more than half the bucket's frames
  frames       integer NOT NULL DEFAULT 0,      -- frames averaged; a rewrite never replaces a fuller row
  PRIMARY KEY (session_id, bucket_start)
);

-- The signal around each marker, one row per marker ("trial"): band power in
-- short windows from before the marker to after it, and the raw signal around
-- it. Averaged per label by GET /sessions/{id}/responses and /erp.
CREATE TABLE IF NOT EXISTS epochs (
  session_id     text NOT NULL REFERENCES sessions(id),
  marker_sample  bigint NOT NULL,               -- the marker's position in the session
  label          text NOT NULL,
  at             timestamptz NOT NULL,          -- the marker's time
  steps          integer NOT NULL,              -- windows in the epoch
  pre_steps      integer NOT NULL,              -- how many of them end at or before the marker
  step_seconds   real NOT NULL,                 -- time between two windows
  window_seconds real NOT NULL,                 -- length of one window
  band_power     real[] NOT NULL,               -- steps x channels x 5 bands, absolute
  bad            boolean[] NOT NULL,            -- per channel: flat or noisy somewhere in the epoch
  PRIMARY KEY (session_id, marker_sample)       -- makes a recomputed epoch harmless
);
-- The raw signal around the marker, for the ERP (GET /sessions/{id}/erp).
-- Empty on epochs stored before it existed.
ALTER TABLE epochs ADD COLUMN IF NOT EXISTS voltage real[] NOT NULL DEFAULT '{}';  -- voltage_samples x channels, uV
ALTER TABLE epochs ADD COLUMN IF NOT EXISTS voltage_samples integer NOT NULL DEFAULT 0;
ALTER TABLE epochs ADD COLUMN IF NOT EXISTS voltage_pre integer NOT NULL DEFAULT 0; -- the marker's row in voltage

-- Replays the api asked for (samples and uploads). The replay worker claims
-- queued rows. session_id has no foreign key: the gateway creates the session
-- row only when the replay starts streaming.
CREATE TABLE IF NOT EXISTS replays (
  session_id text PRIMARY KEY,
  kind       text NOT NULL,                     -- sample | upload
  label      text NOT NULL,                     -- shown on the page; never used as a file name
  object_key text NOT NULL DEFAULT '',          -- storage key, for uploads
  sample     text NOT NULL DEFAULT '',          -- sample file name, e.g. S001R02.edf
  speed      text NOT NULL DEFAULT '1x',        -- 1x | fast
  flatten    text NOT NULL DEFAULT '',          -- CHANNEL@SECONDS, or empty
  start_seconds real NOT NULL DEFAULT 0,        -- where in the recording to start ("replay from here")
  end_seconds   real NOT NULL DEFAULT 0,        -- where to stop; 0 plays to the end
  status     text NOT NULL DEFAULT 'queued',    -- queued | running | done | failed
  error      text NOT NULL DEFAULT '',
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now()
);
-- For databases created before start_seconds existed.
ALTER TABLE replays ADD COLUMN IF NOT EXISTS start_seconds real NOT NULL DEFAULT 0;
ALTER TABLE replays ADD COLUMN IF NOT EXISTS end_seconds real NOT NULL DEFAULT 0;
CREATE INDEX IF NOT EXISTS replays_queued ON replays (created_at) WHERE status = 'queued';

CREATE TABLE IF NOT EXISTS summaries (
  session_id text PRIMARY KEY REFERENCES sessions(id),
  body       text NOT NULL,
  model      text NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now()
);
