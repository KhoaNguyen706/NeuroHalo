// The recorder keeps the raw signal of every session. It reads eeg.raw as its
// own consumer group, next to the processor, so recording never slows the
// live view. Every couple of seconds it uploads each session's new batches to
// the object store as one chunk, and commits the Kafka offsets only after
// that. When a session ends, finalize.go merges its chunks into an EDF+ file.
package main

import (
	"bytes"
	"context"
	"errors"
	"log"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/twmb/franz-go/pkg/kgo"
	"google.golang.org/protobuf/proto"

	"github.com/neurohalo/neurohalo/internal/metrics"

	neurov1 "github.com/neurohalo/neurohalo/gen/neuro/v1"
)

// The recorder's Prometheus metrics (the file metrics are in finalize.go).
var (
	// batchesTaken counts batches added to a chunk. Raised by: recorder.handle.
	batchesTaken = promauto.NewCounter(prometheus.CounterOpts{
		Name: "recorder_batches_total",
		Help: "Batches added to a chunk.",
	})
	// batchesResent counts batches dropped because their seq was already taken.
	// Raised by: recorder.handle.
	batchesResent = promauto.NewCounter(prometheus.CounterOpts{
		Name: "recorder_duplicate_batches_total",
		Help: "Batches dropped because their seq was already taken.",
	})
	// chunksUploaded counts chunks stored. Raised by: recorder.upload.
	chunksUploaded = promauto.NewCounter(prometheus.CounterOpts{
		Name: "recorder_chunks_total",
		Help: "Chunks stored in the object store.",
	})
	// chunkBytes counts the bytes of those chunks. Raised by: recorder.upload.
	chunkBytes = promauto.NewCounter(prometheus.CounterOpts{
		Name: "recorder_chunk_bytes_total",
		Help: "Bytes of the chunks stored in the object store.",
	})
)

const (
	rawTopic = "eeg.raw"
	group    = "recorder"

	pollWait       = 500 * time.Millisecond // how long one poll waits, so flushes happen on time when Kafka is quiet
	maxBuffered    = 32 << 20               // flush early when this many bytes wait
	uploadWorkers  = 8
	sessionTimeout = 10 * time.Second
	idleSessionTTL = 10 * time.Minute
	dbTimeout      = 5 * time.Second
)

// recorder holds the connections and what it remembers per session.
// Created by: main.
// Used by: loadSession, handle, upload, flush, saveProgress, run (this file);
// finalizeLoop, claim, finalize, writeFile (finalize.go).
type recorder struct {
	kafka       *kgo.Client
	db          *pgxpool.Pool
	store       *minio.Client
	bucket      string
	skipSources map[string]bool
	flushEvery  time.Duration
	idleAfter   time.Duration // a session silent this long is written even without a clean end

	sessions map[string]*sessionState
	buffered int // bytes waiting in chunks
}

// sessionState is what the recorder remembers about one session. It is only
// a cache: after a restart the chunks and the recordings row hold everything.
// Created by: recorder.handle. Used by: handle, flush, run.
type sessionState struct {
	record   bool   // false for a session that is not in Postgres or comes from a skipped source
	haveSeq  bool   // whether maxSeq is set
	maxSeq   uint64 // the highest seq taken, so resent batches are dropped before they are stored
	pending  *chunk // batches not uploaded yet; nil when there are none
	lastSeen time.Time
}

// retry runs fn until it succeeds or ctx ends, waiting longer after each
// failure (0.5 s, doubling up to 10 s). While it waits nothing is committed,
// so Kafka keeps the data.
func retry(ctx context.Context, what string, fn func(context.Context) error) error {
	backoff := 500 * time.Millisecond
	for {
		err := fn(ctx)
		if err == nil || ctx.Err() != nil {
			return err
		}
		log.Printf("%s failed, retrying in %v: %v", what, backoff, err)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 10*time.Second)
	}
}

// loadSession decides whether a session is recorded: it must be in Postgres
// (the gateway saved it when the stream opened) and its source must not be
// skipped. It keeps trying while Postgres is down.
func (r *recorder) loadSession(ctx context.Context, id string) (bool, error) {
	var source string
	err := retry(ctx, "session "+id+": load", func(ctx context.Context) error {
		qctx, cancel := context.WithTimeout(ctx, dbTimeout)
		defer cancel()
		err := r.db.QueryRow(qctx, `SELECT source FROM sessions WHERE id = $1`, id).Scan(&source)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return err
	})
	switch {
	case err != nil:
		return false, err
	case source == "":
		log.Printf("session %s: not in Postgres, not recorded", id)
		return false, nil
	case r.skipSources[source]:
		log.Printf("session %s: source %q is not recorded", id, source)
		return false, nil
	}
	return true, nil
}

// handle takes one record from eeg.raw: it adds the batch to its session's
// chunk, unless the session is not recorded or the batch was already taken.
func (r *recorder) handle(ctx context.Context, rec *kgo.Record) error {
	var b neurov1.SampleBatch
	if err := proto.Unmarshal(rec.Value, &b); err != nil {
		log.Printf("partition %d offset %d: not a SampleBatch, skipped: %v", rec.Partition, rec.Offset, err)
		return nil
	}
	st := r.sessions[b.SessionId]
	if st == nil {
		record, err := r.loadSession(ctx, b.SessionId)
		if err != nil {
			return err
		}
		st = &sessionState{record: record}
		r.sessions[b.SessionId] = st
	}
	st.lastSeen = time.Now()
	if !st.record {
		return nil
	}
	if st.haveSeq && b.Seq <= st.maxSeq {
		batchesResent.Inc()
		return nil
	}
	st.haveSeq, st.maxSeq = true, b.Seq
	if st.pending == nil {
		st.pending = &chunk{}
	}
	st.pending.add(b.Seq, rec.Value)
	r.buffered += len(rec.Value)
	batchesTaken.Inc()
	return nil
}

// upload stores one chunk, retrying until it succeeds.
func (r *recorder) upload(ctx context.Context, id string, c *chunk) error {
	key := chunkKey(id, c)
	return retry(ctx, "upload "+key, func(ctx context.Context) error {
		_, err := r.store.PutObject(ctx, r.bucket, key, bytes.NewReader(c.buf.Bytes()), int64(c.buf.Len()),
			minio.PutObjectOptions{ContentType: "application/octet-stream"})
		if err == nil {
			chunksUploaded.Inc()
			chunkBytes.Add(float64(c.buf.Len()))
		}
		return err
	})
}

// saveProgress records, in one statement, the highest seq now stored for
// each session. A higher seq than before puts a finished recording back to
// "recording" and clears an earlier failure, so the file is written again
// with the new batches. Sessions deleted in the meantime are left out.
func (r *recorder) saveProgress(ctx context.Context, ids []string, seqs []int64) error {
	return retry(ctx, "save progress", func(ctx context.Context) error {
		qctx, cancel := context.WithTimeout(ctx, dbTimeout)
		defer cancel()
		_, err := r.db.Exec(qctx, `
			INSERT INTO recordings (session_id, saved_seq)
			SELECT u.id, u.seq FROM unnest($1::text[], $2::bigint[]) AS u(id, seq)
			WHERE EXISTS (SELECT 1 FROM sessions s WHERE s.id = u.id)
			ON CONFLICT (session_id) DO UPDATE SET
				status     = CASE WHEN EXCLUDED.saved_seq > recordings.saved_seq THEN 'recording' ELSE recordings.status END,
				error      = CASE WHEN EXCLUDED.saved_seq > recordings.saved_seq THEN '' ELSE recordings.error END,
				updated_at = CASE WHEN EXCLUDED.saved_seq > recordings.saved_seq THEN now() ELSE recordings.updated_at END,
				saved_seq  = GREATEST(recordings.saved_seq, EXCLUDED.saved_seq)`,
			ids, seqs)
		return err
	})
}

// flush uploads every waiting chunk (several at a time), records the
// progress in Postgres, and only then commits the Kafka offsets. A crash
// before the commit makes Kafka deliver the batches again; the merge drops
// the copies by seq, so nothing is lost and nothing is doubled.
func (r *recorder) flush(ctx context.Context) error {
	var ids []string
	var seqs []int64
	var chunks []*chunk
	for id, st := range r.sessions {
		if st.pending != nil {
			ids, seqs, chunks = append(ids, id), append(seqs, int64(st.pending.lastSeq)), append(chunks, st.pending)
		}
	}
	if len(chunks) > 0 {
		var wg sync.WaitGroup
		errs := make([]error, len(chunks))
		next := make(chan int)
		for range min(uploadWorkers, len(chunks)) {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := range next {
					errs[i] = r.upload(ctx, ids[i], chunks[i])
				}
			}()
		}
		for i := range chunks {
			next <- i
		}
		close(next)
		wg.Wait()
		if err := errors.Join(errs...); err != nil {
			return err
		}
		if err := r.saveProgress(ctx, ids, seqs); err != nil {
			return err
		}
	}
	if err := r.kafka.CommitUncommittedOffsets(ctx); err != nil {
		log.Printf("commit: %v", err) // the next flush commits these offsets too
	}
	for _, id := range ids {
		r.sessions[id].pending = nil
	}
	r.buffered = 0
	return nil
}

// run is the main loop: poll, hand each record to handle, and flush every
// flushEvery (or sooner when maxBuffered bytes wait). Sessions idle for 10
// minutes are forgotten. On shutdown it flushes once more, so a restart does
// not have to read those batches again.
func (r *recorder) run(ctx context.Context) error {
	lastFlush, lastSweep := time.Now(), time.Now()
	for {
		pctx, cancel := context.WithTimeout(ctx, pollWait)
		fetches := r.kafka.PollFetches(pctx)
		cancel()
		if ctx.Err() != nil || fetches.IsClientClosed() {
			fctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			return r.flush(fctx)
		}
		fetches.EachError(func(topic string, partition int32, err error) {
			if !errors.Is(err, context.DeadlineExceeded) {
				log.Printf("fetch %s/%d: %v", topic, partition, err)
			}
		})
		var handleErr error
		fetches.EachRecord(func(rec *kgo.Record) {
			if handleErr == nil {
				handleErr = r.handle(ctx, rec)
			}
		})
		if handleErr != nil {
			return handleErr
		}

		now := time.Now()
		if now.Sub(lastFlush) >= r.flushEvery || r.buffered >= maxBuffered {
			if err := r.flush(ctx); err != nil {
				return err
			}
			lastFlush = now
		}
		if now.Sub(lastSweep) >= time.Minute {
			for id, st := range r.sessions {
				if st.pending == nil && now.Sub(st.lastSeen) > idleSessionTTL {
					delete(r.sessions, id)
				}
			}
			lastSweep = now
		}
	}
}

// ensureBucket creates the bucket if needed, retrying while the store starts up.
func (r *recorder) ensureBucket(ctx context.Context) error {
	return retry(ctx, "bucket "+r.bucket, func(ctx context.Context) error {
		exists, err := r.store.BucketExists(ctx, r.bucket)
		if err == nil && !exists {
			err = r.store.MakeBucket(ctx, r.bucket, minio.MakeBucketOptions{})
		}
		return err
	})
}

// env returns an environment variable, or fallback when it is unset or empty.
func env(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

// duration reads an environment variable as a duration ("2s"), or fallback.
func duration(name string, fallback time.Duration) time.Duration {
	if d, err := time.ParseDuration(os.Getenv(name)); err == nil && d > 0 {
		return d
	}
	return fallback
}

// main connects to Kafka (as consumer group "recorder"), Postgres and the
// object store, serves Prometheus metrics on METRICS_ADDR, starts the
// finalizer, and runs the loop until Ctrl+C or SIGTERM.
func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	metrics.Serve(env("METRICS_ADDR", ":9100"))

	store, err := minio.New(env("S3_ENDPOINT", "localhost:8333"), &minio.Options{
		Creds:  credentials.NewStaticV4(os.Getenv("S3_ACCESS_KEY"), os.Getenv("S3_SECRET_KEY"), ""),
		Secure: os.Getenv("S3_SECURE") == "true",
	})
	if err != nil {
		log.Fatalf("object store: %v", err)
	}

	db, err := pgxpool.New(ctx, env("DATABASE_URL", "postgres://neuro:neuro@localhost:5432/neuro"))
	if err != nil {
		log.Fatalf("postgres: %v", err)
	}
	defer db.Close()

	kafka, err := kgo.NewClient(
		kgo.SeedBrokers(strings.Split(env("KAFKA_BROKERS", "localhost:29092"), ",")...),
		kgo.ConsumerGroup(group),
		kgo.ConsumeTopics(rawTopic),
		kgo.DisableAutoCommit(),
		kgo.SessionTimeout(sessionTimeout),
		// With no committed offset yet (the first start), begin at the newest
		// batch. The oldest batches in eeg.raw are what retention has not
		// deleted yet, so starting there would write files that silently miss
		// the start of their session.
		kgo.ConsumeResetOffset(kgo.NewOffset().AtEnd()),
	)
	if err != nil {
		log.Fatalf("kafka: %v", err)
	}
	defer kafka.Close()

	r := &recorder{
		kafka: kafka, db: db, store: store,
		bucket:      env("S3_BUCKET", "recordings"),
		skipSources: map[string]bool{},
		flushEvery:  duration("FLUSH_EVERY", 2*time.Second),
		idleAfter:   duration("IDLE_AFTER", 2*time.Minute),
		sessions:    map[string]*sessionState{},
	}
	// The load generator's sessions are not recorded by default: a load test
	// would otherwise store gigabytes of the same minute of signal.
	for _, s := range strings.Split(env("SKIP_SOURCES", "loadgen"), ",") {
		if s = strings.TrimSpace(s); s != "" {
			r.skipSources[s] = true
		}
	}
	if err := r.ensureBucket(ctx); err != nil {
		log.Fatalf("object store: %v", err)
	}
	go r.finalizeLoop(ctx)

	log.Printf("recorder started, group %q reading %s, flushing every %v", group, rawTopic, r.flushEvery)
	if err := r.run(ctx); err != nil && ctx.Err() == nil {
		log.Fatalf("recorder stopped: %v", err)
	}
	log.Print("recorder stopped")
}
