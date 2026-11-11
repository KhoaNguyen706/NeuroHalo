-- Postgres holds sessions, events, 10-second rollups and summaries.
-- Raw samples are not stored; the recording files remain the source.

CREATE TABLE IF NOT EXISTS sessions (
  id             text PRIMARY KEY,
  source         text NOT NULL,
  sample_rate_hz double precision NOT NULL,
  channel_names  text[] NOT NULL,
  started_at     timestamptz NOT NULL,
  ended_at       timestamptz,
  status         text NOT NULL DEFAULT 'live'   -- live | ended
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
