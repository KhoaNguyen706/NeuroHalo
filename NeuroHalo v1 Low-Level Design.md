# NeuroHalo v1: Low-Level Design

Nov 9, 2026 · @Khoa Nguyen

## Scope

v1 replays public EEG recordings as live streams, shows them on a 3D head, flags bad sensors, and stores session history.

| In v1 | Not in v1 |
| --- | --- |
| Recordings replayed at real speed, from built-in samples or an upload | Real headsets or a lab bridge |
| Rhythm strength per sensor, live | Any trained model |
| Bad-sensor flags | Medical or diagnostic claims |
| Session history in Postgres | User accounts and billing |
| A load test that produces a p99 number | Support for more than one dataset |
| LLM session summary (last milestone) | Raw samples stored in a database |

Working numbers, confirmed on the [dataset page](https://physionet.org/content/eegmmidb/1.0.0/): 64 sensors at 160 samples a second.

- One session is 10,240 values a second, about 41 KB a second.
- 1,000 sessions is about 41 MB a second and 10,000 Kafka messages a second.

## Architecture at a glance

A recording file becomes a stream, the stream becomes rhythm strength and events, and those go to the screen and the database at the same time.

1. A session starts from a built-in sample, an uploaded file, or the replayer run by hand.
2. An uploaded file is saved to file storage.
3. The replayer sends the file to the gateway over gRPC in 100 ms batches.
4. The gateway validates each batch and publishes it to `eeg.raw`.
5. The processor reads raw batches and publishes rhythm strength and quality to `eeg.features`, and events to `eeg.events`.
6. The api reads those topics with two consumers. One pushes frames to browsers over WebSocket. The other writes events and rollups to Postgres.
7. The web app draws the 3D head and the session page.
8. The summarizer reads Postgres to write a summary and answer MCP queries.

Step 6 splits so the live view never waits on the database.

## Tech stack

Every choice below is free to run locally. Only the short AWS run at the end costs money.

| Layer | Choice | Why |
| --- | --- | --- |
| Backend services | Go | One backend language, strong at many concurrent streams |
| Replayer, summarizer | Python | Best libraries for EEG files and LLM calls |
| Between services | gRPC, Protocol Buffers | Typed contract with streaming built in |
| Event stream | Kafka in KRaft mode | Ordered per session, replayable, several independent consumers |
| Database | PostgreSQL | Sessions, events, rollups, summaries |
| File storage | MinIO locally, S3 on AWS | Recording files |
| Live updates | WebSocket | Server pushes frames to the browser |
| Frontend | Next.js, React Three Fiber | You know Next.js; 3D written as React components |
| Signal math | gonum FFT | Runs inside the Go processor |
| Local development | Docker Compose | Whole stack starts with one command |
| Orchestration | Kubernetes on kind, Helm | Free locally; the same charts run in the cloud |
| Cloud | AWS, short run, Terraform optional | Final numbers, then tear down |
| Monitoring | Prometheus, Grafana | Latency histograms and consumer lag |
| Load testing | Custom Go generator | Holds 1,000 client-streaming gRPC sessions |
| CI | GitHub Actions | Tests and image builds on every push |
| AI | LLM API, MCP server | Summaries and agent queries, no trained model |

## Services

Six small services, each with one job. Data flows top to bottom in this table.

| Service | Language | Job | Talks to |
| --- | --- | --- | --- |
| replayer | Python | Reads a recording file and sends it at real speed; run by hand in development, and as a worker for samples and uploads | gateway (gRPC) |
| gateway | Go | Checks the token, validates batches, publishes them | Kafka, Postgres |
| processor | Go | Turns raw samples into rhythm strength, quality flags and events | Kafka |
| api | Go | Serves history over REST, pushes live frames over WebSocket, writes events and rollups, accepts uploads | Kafka, Postgres |
| web | Next.js, React Three Fiber | 3D head, session list, event list | api |
| summarizer | Python | LLM session summary and MCP server (last milestone) | Postgres |

Two rules keep the boundaries clean:

- The gateway never computes anything. It validates and forwards.
- The processor never talks to clients. It reads Kafka and writes Kafka.

## Ingest contract

Every data source sends this one format over a client-streaming gRPC call. A header comes first, then batches.

```proto
syntax = "proto3";
package neuro.v1;

message SessionHeader {
  string session_id = 1;
  double sample_rate_hz = 2;
  repeated string channel_names = 3;   // standard names, e.g. "Fp1", "Cz"
  string source = 4;                   // e.g. "replayer"
}

message SampleBatch {
  string session_id = 1;
  uint64 seq = 2;                      // starts at 0, +1 per batch
  uint64 first_sample_index = 3;       // position in the session
  int64 sent_time_unix_nanos = 4;      // wall clock when the sender sent it
  uint32 num_samples = 5;
  repeated float values = 6;           // num_samples x channels, row by row, microvolts
}

message IngestRequest {
  oneof msg {
    SessionHeader header = 1;
    SampleBatch batch = 2;
  }
}

message IngestSummary {
  uint64 batches_received = 1;
  uint64 last_seq = 2;
}

service Ingest {
  rpc Stream(stream IngestRequest) returns (IngestSummary);
}
```

Rules the gateway enforces:

- The first message on a stream must be a header. Otherwise it returns `INVALID_ARGUMENT`.
- `values` must hold exactly `num_samples` times the channel count.
- A batch covers 100 ms, which is 16 samples at 160 samples a second.
- The token travels in gRPC metadata as `authorization: Bearer <token>`. v1 uses one static token from an environment variable.
- After a disconnect, the client opens a new stream, resends the header with the same `session_id`, and continues `seq` where it stopped.

## Kafka

Three topics, all keyed by `session_id` so each session's data stays in order.

| Topic | Value | Partitions | Retention |
| --- | --- | --- | --- |
| `eeg.raw` | `SampleBatch` | 12 | Capped by size, start at 1 GB per partition |
| `eeg.features` | `FeatureFrame` | 12 | 24 hours |
| `eeg.events` | `Event` | 3 | 7 days |

Raw retention is capped by size because raw data is large. 1,000 sessions for one hour would be about 147 GB.

Delivery rules:

- Producers use `acks=all` with idempotence turned on.
- Consumers are at-least-once. They commit offsets only after their output is written.
- The processor remembers the last `seq` per session and drops any batch at or below it. That removes duplicates.

Consumer groups:

| Group | Reads | Why |
| --- | --- | --- |
| `processor` | `eeg.raw` | Shared group, so adding processors spreads the partitions |
| `api-store` | `eeg.features`, `eeg.events` | Shared group, writes to Postgres once |
| `api-live-<instance>` | `eeg.features`, `eeg.events` | One group per api instance, so every instance sees every frame |

Session details (sample rate, channel names) do not travel in every Kafka message. The gateway saves them to Postgres when the header arrives. The processor loads them on a session's first batch and caches them.

## Stream processor

For each sensor, the processor keeps the last 2 seconds of signal and recomputes every 0.5 seconds.

Each recompute does three things per sensor:

1. Applies a Hann window and runs an FFT on the 2-second buffer. At 160 samples a second that is 320 samples, giving 0.5 Hz resolution.
2. Sums power in each rhythm band and divides by total power from 1 to 45 Hz. The result is relative strength from 0 to 1.
3. Checks signal quality.

| Band | Range (Hz) |
| --- | --- |
| Delta | 1 to 4 |
| Theta | 4 to 8 |
| Alpha | 8 to 13 |
| Beta | 13 to 30 |
| Gamma | 30 to 45 |

These are common ranges. Labs vary them slightly.

| Quality | Rule (starting guess, tune on real data) |
| --- | --- |
| Flat | Standard deviation below 0.5 microvolts |
| Noisy | Peak-to-peak above 200 microvolts |
| OK | Neither of the above |

| Event | Fires when |
| --- | --- |
| `channel_bad` | A sensor is flat or noisy for 3 windows in a row |
| `channel_recovered` | A bad sensor is OK for 3 windows in a row |
| `alpha_rise` | Mean alpha over back-of-head sensors (names starting with O, PO or P) stays above 1.5 times the session's running average for 4 windows |

Output messages:

```proto
enum Quality {
  QUALITY_UNSPECIFIED = 0;
  QUALITY_OK = 1;
  QUALITY_FLAT = 2;
  QUALITY_NOISY = 3;
}

message FeatureFrame {
  string session_id = 1;
  uint64 window_index = 2;
  int64 newest_sent_time_unix_nanos = 3;  // copied from the newest batch in the window
  repeated float band_power = 4;          // channels x 5 bands, row by row
  repeated Quality quality = 5;           // one per channel
}

message Event {
  string session_id = 1;
  string type = 2;
  string channel = 3;                     // empty when the event covers the whole head
  int64 time_unix_nanos = 4;
  string details_json = 5;
}
```

State rules for v1:

- A gap in `seq` clears that session's buffers and adds one to a missing-batch counter.
- After a restart or a rebalance, buffers start empty and refill within 2 seconds. There is no saved state.
- In Go, gonum's `dsp/fourier` package provides the FFT.

## Postgres schema

Postgres holds sessions, events, 10-second rollups and summaries. Raw samples are not stored; the recording files remain the source.

```sql
CREATE TABLE sessions (
  id             text PRIMARY KEY,
  source         text NOT NULL,
  sample_rate_hz double precision NOT NULL,
  channel_names  text[] NOT NULL,
  started_at     timestamptz NOT NULL,
  ended_at       timestamptz,
  status         text NOT NULL DEFAULT 'live'   -- live | ended
);

CREATE TABLE events (
  id         bigserial PRIMARY KEY,
  session_id text NOT NULL REFERENCES sessions(id),
  type       text NOT NULL,
  channel    text NOT NULL DEFAULT '',
  at         timestamptz NOT NULL,
  details    jsonb NOT NULL DEFAULT '{}',
  UNIQUE (session_id, type, channel, at)        -- makes replays of the same event harmless
);
CREATE INDEX events_session_at ON events (session_id, at);

CREATE TABLE rollups (
  session_id   text NOT NULL REFERENCES sessions(id),
  bucket_start timestamptz NOT NULL,            -- 10-second buckets
  band_power   real[] NOT NULL,                 -- channels x 5 bands, averaged over the bucket
  bad_channels text[] NOT NULL DEFAULT '{}',
  PRIMARY KEY (session_id, bucket_start)
);

CREATE TABLE summaries (
  session_id text PRIMARY KEY REFERENCES sessions(id),
  body       text NOT NULL,
  model      text NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now()
);
```

Writes are idempotent. Events use `ON CONFLICT DO NOTHING` and rollups use `ON CONFLICT DO UPDATE`, so a redelivered Kafka message changes nothing.

One rollup row per session per bucket keeps the write rate low: 1,000 sessions produce 100 rows a second.

## API and WebSocket

The api service answers history questions over REST and pushes live frames over one WebSocket per viewer.

| Method | Path | Returns |
| --- | --- | --- |
| GET | `/sessions` | Sessions, newest first, with status |
| GET | `/sessions/{id}` | One session with its channel names and sample rate |
| GET | `/sessions/{id}/events` | Events in time order |
| GET | `/sessions/{id}/rollups?from=&to=` | Rollup rows in a time range |
| POST | `/sessions/{id}/summary` | Creates or returns the LLM summary |
| WS | `/ws/sessions/{id}/live` | Live frames and events |

The viewer picks which band to watch:

```json
{ "type": "subscribe", "band": "alpha" }
```

The server then sends one frame every 0.5 seconds, plus events as they happen:

```json
{ "type": "frame", "windowIndex": 412, "sentTimeMs": 1794300000000,
  "band": "alpha", "power": [0.31, 0.28], "quality": [1, 1] }

{ "type": "event", "eventType": "channel_bad", "channel": "Cz", "atMs": 1794300002500 }
```

`power` and `quality` hold one value per channel, in the session's channel order.

Rules:

- Each viewer has a buffer of one frame. A slow viewer skips to the newest frame and never slows the others.
- v1 uses the same static bearer token as the gateway.

## 3D dashboard

The dashboard is a head shape with one dot per sensor. Dot brightness shows rhythm strength and a red ring marks a bad sensor.

- **Head:** a slightly stretched sphere. No model file is needed.
- **Positions:** a JSON table from channel name to x, y, z. Generate it once from MNE's standard sensor layout (`standard_1005`) and commit the file.
- **Names:** the dataset may pad channel names with dots or use odd casing. The replayer normalizes them before sending. Check this when you first print a file.
- **Dots:** one instanced mesh for all sensors. A sensor with no known position is drawn grey in a row below the head.
- **Color:** relative power of the selected band, dim for low and bright for high.
- **Smoothing:** frames arrive twice a second, so the page blends colors between frames.

Side panel:

- Band selector
- Event list, newest first
- Delay in milliseconds, computed as browser time minus `sentTimeMs`

The on-screen delay is only accurate when the replayer and the browser share a clock, which is true when both run on your laptop. The load-test latency is measured on the server instead.

## Replayer and load generator

Two programs turn a recording file into streams: a Python replayer for demos and a Go load generator for scale.

**Replayer (Python).** It handles one session and is easy to change.

1. Read the EDF file with MNE.
2. Normalize channel names and convert values to microvolts as 32-bit floats.
3. Send the header.
4. Send one 100 ms batch at a time, scheduled against a monotonic clock so timing does not drift.
5. On disconnect, reconnect with backoff and continue `seq`.

| Flag | Purpose |
| --- | --- |
| `--file` | Recording to replay |
| `--gateway` | Gateway address |
| `--session-id` | Defaults to a random ID |
| `--speed` | 1.0 is real time |
| `--flatten CHANNEL@SECONDS` | Zeroes one sensor from that moment on, to demo bad-sensor detection |

**Load generator (Go).** Python will not hold 1,000 smooth streams, so load comes from Go.

1. Convert one recording once into a flat 32-bit float file plus a small JSON header.
2. Open N gRPC streams with staggered starts, each with its own `session_id`.
3. Replay the same data on every stream at real speed.

k6 also supports gRPC. Check whether it handles client streaming well before choosing it over a custom generator.

## Samples and uploads

A session can start three ways, and all three feed the same pipeline.

| Start | How |
| --- | --- |
| Built-in sample | `POST /samples/{name}/play` starts a replay of a bundled recording |
| Upload | `POST /uploads` accepts one EDF file, saves it to file storage, then starts a replay |
| By hand | Run the replayer from the terminal during development |

Three samples cover the demo, all from one subject's baseline runs:

- Eyes open (run 1)
- Eyes closed (run 2)
- Eyes closed with one sensor flattened partway through

Upload rules:

- EDF files only, with a 50 MB cap. A two-minute run in this dataset is 2.4 MB.
- Parse the file header before accepting. Reject files that fail to parse.
- Store each file under a generated name, never the name the user sent.

Each replay has a speed setting. `1x` plays it as it happened. `fast` processes it as quickly as the pipeline allows, for reviewing old recordings. Pipeline latency is only meaningful at `1x`.

The dataset is under the Open Data Commons Attribution License v1.0. Read the license before bundling files in the repo, and credit the dataset in the README.

Build samples after step 7 and upload after that.

## Metrics

Pipeline latency is the time the api receives a `FeatureFrame` minus the `newest_sent_time` inside it.

That covers the gateway, both Kafka hops and the processor. It is measured from the newest batch in the window, so it leaves out the planned 0.5-second wait between recomputes.

| Service | Metric | Type |
| --- | --- | --- |
| gateway | `ingest_batches_total` | Counter |
| gateway | `ingest_bytes_total` | Counter |
| gateway | `active_streams` | Gauge |
| gateway | `rejected_batches_total` by reason | Counter |
| processor | `window_compute_seconds` | Histogram |
| processor | `missing_batches_total` | Counter |
| processor | `duplicate_batches_total` | Counter |
| api | `pipeline_latency_seconds` | Histogram |
| api | `ws_clients` | Gauge |
| api | `ws_frames_dropped_total` | Counter |
| Kafka | Consumer lag per group | Gauge |

Numbers to report, each at 10, 100, 500 and 1,000 sessions:

- Batches a second and MB a second through the gateway
- p50 and p99 pipeline latency
- Consumer lag for the `processor` group
- The session count where p99 starts climbing

Measure first, then set a target. A target picked before measuring is a guess.

## Failure cases

Every failure below should leave the live view running or recover within seconds, without losing stored events.

| What breaks | What the system does |
| --- | --- |
| Gateway restarts | Replayer reconnects with backoff, resends the header, continues `seq` |
| Kafka is unreachable | Gateway returns `UNAVAILABLE`; the replayer backs off and retries |
| Processor crashes | Kafka reassigns its partitions; buffers refill in 2 seconds |
| Same batch delivered twice | Processor drops it by `seq` |
| A batch goes missing | Processor clears that session's buffers and counts the gap |
| Postgres is down | The api retries writes and holds its Kafka offsets; the live view keeps working |
| A viewer is slow | It skips to the newest frame; other viewers are unaffected |
| A sensor goes flat | `channel_bad` fires within about 1.5 seconds and the dot turns red |
| A batch has the wrong size | Gateway rejects it with `INVALID_ARGUMENT` and counts it |

Demo these two on video: kill the processor mid-session, and flatten a sensor with `--flatten`.

## Build order

Build in this order and do not start a step until the one before it runs. After step 4 you have a pipeline you can demo in a terminal.

- [ ] 1\. Load one EDF file in Python. Print channel names, sample rate and the first rows.
- [ ] 2\. Write the `.proto` file. Gateway accepts a stream and logs batch counts.
- [ ] 3\. Replayer sends at real speed. Gateway publishes to `eeg.raw`, with Kafka and Postgres in Docker Compose.
- [ ] 4\. Processor writes `eeg.features`. Check in the terminal that back-of-head alpha rises in the eyes-closed run.
- [ ] 5\. Api stores events and rollups and serves the REST endpoints.
- [ ] 6\. WebSocket plus the 3D head.
- [ ] 7\. Bad-sensor demo with `--flatten`.
- [ ] 8\. Prometheus metrics and a Grafana dashboard.
- [ ] 9\. Go load generator. Record the numbers at 10, 100, 500 and 1,000 sessions.
- [ ] 10\. Kubernetes on kind with Helm. Kill a processor pod mid-session.
- [ ] 11\. Summarizer and MCP server.
- [ ] 12\. README, demo video, and a short AWS run for the final numbers.

Step 4 is the first real test of the design. If the eyes-closed effect shows up, the math and the pipeline both work.

## Open decisions

Six choices are left to you. The design above assumes the first option in each row, and you should be able to defend whichever you pick.

| Decision | Options | What changes |
| --- | --- | --- |
| Batch size | 100 ms or 250 ms | Smaller batches cut latency but send 2.5 times more messages |
| Processor language | Go or Python | Go keeps one backend language and runs faster; Python makes the signal math easier |
| Rollup bucket | 10 seconds or 1 second | Smaller buckets give finer history and 10 times more rows |
| Quality thresholds | The starting guesses or values tuned on real data | Wrong thresholds mean false alarms or missed bad sensors |
| Store writer | Inside the api service or its own service | One fewer service to run, but live traffic and storage share a process |
| Project name | NeuroHalo (working name) | The repo, the README and the resume line |

Questions to answer before step 2, in your own words:

- [ ] Why key Kafka by `session_id` and not by channel?
- [ ] What would break if the processor committed offsets before writing its output?
- [ ] Why does the gateway reject a wrong-size batch when it could just pass it on?
