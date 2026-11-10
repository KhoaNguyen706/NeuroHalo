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

CREATE TABLE IF NOT EXISTS summaries (
  session_id text PRIMARY KEY REFERENCES sessions(id),
  body       text NOT NULL,
  model      text NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now()
);
