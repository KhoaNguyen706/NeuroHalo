// The processor turns raw samples into rhythm strength, quality flags and
// events. It never talks to clients: it reads Kafka and writes Kafka.
package main

import (
	"context"
	"errors"
	"log"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/twmb/franz-go/pkg/kgo"
	"google.golang.org/protobuf/proto"

	"github.com/neurohalo/neurohalo/internal/metrics"

	neurov1 "github.com/neurohalo/neurohalo/gen/neuro/v1"
)

// The processor's Prometheus metrics (links_compute_seconds is in links.go).
var (
	// windowComputeSeconds times one window: band power, quality and links for every sensor.
	// Observed by: session.add.
	windowComputeSeconds = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "window_compute_seconds",
		Help:    "Time to compute one 2-second window of one session.",
		Buckets: prometheus.ExponentialBuckets(0.0001, 2, 12), // 0.1 ms to about 0.2 s
	})
	// missingBatches counts jumps in seq. Raised by: processor.handle.
	missingBatches = promauto.NewCounter(prometheus.CounterOpts{
		Name: "missing_batches_total",
		Help: "Gaps in a session's batch sequence.",
	})
	// duplicateBatches counts batches dropped as already seen. Raised by: processor.handle.
	duplicateBatches = promauto.NewCounter(prometheus.CounterOpts{
		Name: "duplicate_batches_total",
		Help: "Batches dropped because their seq was already processed.",
	})
)

const (
	rawTopic      = "eeg.raw"
	featuresTopic = "eeg.features"
	eventsTopic   = "eeg.events"
	group         = "processor"

	idleSessionTTL = 10 * time.Minute
	statsEvery     = 10 * time.Second
)

// processor holds the Kafka and Postgres connections, the per-session state
// and the counters.
// Created by: main.
// Used by: loadSession, send, handle, run (its methods).
type processor struct {
	kafka    *kgo.Client
	db       *pgxpool.Pool
	sessions map[string]*session // nil value: session is not in Postgres, skip it
	links    *linkConfig         // nil when LINKS_ENABLED is off

	produceErr atomic.Pointer[error]

	batches, frames, events, duplicates, missing, unknown uint64
}

// loadSession reads the details the gateway saved when the header arrived.
// It keeps trying while Postgres is down, which also holds the Kafka offsets.
func (p *processor) loadSession(ctx context.Context, id string) (*session, error) {
	backoff := 500 * time.Millisecond
	for {
		var rate float64
		var names []string
		var startedAt time.Time
		qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err := p.db.QueryRow(qctx,
			`SELECT sample_rate_hz, channel_names, started_at FROM sessions WHERE id = $1`, id).
			Scan(&rate, &names, &startedAt)
		cancel()
		switch {
		case err == nil:
			return newSession(id, rate, names, startedAt, p.links), nil
		case errors.Is(err, pgx.ErrNoRows):
			return nil, nil
		case ctx.Err() != nil:
			return nil, ctx.Err()
		}
		log.Printf("session %s: cannot load details, retrying in %v: %v", id, backoff, err)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 10*time.Second)
	}
}

// send queues one output message for Kafka without waiting for it. The first
// failure is remembered in produceErr, which run checks before it commits.
func (p *processor) send(topic, key string, msg proto.Message) {
	value, err := proto.Marshal(msg)
	if err != nil {
		p.produceErr.CompareAndSwap(nil, &err)
		return
	}
	p.kafka.Produce(context.Background(), &kgo.Record{Topic: topic, Key: []byte(key), Value: value},
		func(_ *kgo.Record, err error) {
			if err != nil {
				p.produceErr.CompareAndSwap(nil, &err)
			}
		})
}

// handle processes one record from eeg.raw: decode the batch, find or load
// its session, feed the samples in, and queue the frames and events that
// come out. It returns an error only when the processor has to stop.
func (p *processor) handle(ctx context.Context, rec *kgo.Record) error {
	var batch neurov1.SampleBatch
	if err := proto.Unmarshal(rec.Value, &batch); err != nil {
		log.Printf("partition %d offset %d: not a SampleBatch, skipped: %v", rec.Partition, rec.Offset, err)
		return nil
	}
	p.batches++

	s, known := p.sessions[batch.SessionId]
	if !known {
		var err error
		if s, err = p.loadSession(ctx, batch.SessionId); err != nil {
			return err
		}
		if s == nil {
			log.Printf("session %s: not in Postgres, its batches are skipped", batch.SessionId)
		}
		p.sessions[batch.SessionId] = s
	}
	if s == nil || len(batch.Values) != int(batch.NumSamples)*s.channels {
		p.unknown++
		return nil
	}

	res := s.add(&batch)
	switch {
	case res.duplicate:
		p.duplicates++
		duplicateBatches.Inc()
		return nil
	case res.gap:
		p.missing++
		missingBatches.Inc()
		log.Printf("session %s: gap before seq %d, buffers cleared", s.id, batch.Seq)
	}
	for _, f := range res.frames {
		p.send(featuresTopic, s.id, f)
	}
	for _, e := range res.events {
		p.send(eventsTopic, s.id, e)
		log.Printf("session %s: event %s %s %s", s.id, e.Type, e.Channel, e.DetailsJson)
	}
	p.frames += uint64(len(res.frames))
	p.events += uint64(len(res.events))
	return nil
}

// run is the main loop: fetch records, handle each one, wait until the output
// is written, then commit the offsets. It also logs the counters every 10
// seconds and forgets sessions that have been idle for 10 minutes.
func (p *processor) run(ctx context.Context) error {
	lastStats, lastSweep := time.Now(), time.Now()
	var lastBatches uint64
	for {
		fetches := p.kafka.PollFetches(ctx)
		if ctx.Err() != nil || fetches.IsClientClosed() {
			return nil
		}
		fetches.EachError(func(topic string, partition int32, err error) {
			log.Printf("fetch %s/%d: %v", topic, partition, err)
		})

		var handleErr error
		fetches.EachRecord(func(rec *kgo.Record) {
			if handleErr == nil {
				handleErr = p.handle(ctx, rec)
			}
		})
		if handleErr != nil {
			return handleErr
		}

		// Commit only after the output is written. Committing first would lose
		// the frames of any batch that was read but not yet published at a crash.
		if err := p.kafka.Flush(ctx); err != nil {
			return err
		}
		if err := p.produceErr.Load(); err != nil {
			return *err
		}
		if err := p.kafka.CommitUncommittedOffsets(ctx); err != nil {
			log.Printf("commit: %v", err)
		}

		now := time.Now()
		if now.Sub(lastStats) >= statsEvery {
			if p.batches != lastBatches {
				log.Printf("batches=%d frames=%d events=%d duplicates=%d missing=%d skipped=%d sessions=%d",
					p.batches, p.frames, p.events, p.duplicates, p.missing, p.unknown, len(p.sessions))
			}
			lastStats, lastBatches = now, p.batches
		}
		if now.Sub(lastSweep) >= time.Minute {
			for id, s := range p.sessions {
				if s == nil || now.Sub(s.lastSeen) > idleSessionTTL {
					delete(p.sessions, id)
				}
			}
			lastSweep = now
		}
	}
}

// env returns an environment variable, or fallback when it is unset or empty.
func env(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

// main connects to Kafka (as consumer group "processor") and Postgres, serves
// Prometheus metrics on METRICS_ADDR, then runs the loop until Ctrl+C or SIGTERM.
func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	links, err := loadLinkConfig()
	if err != nil {
		log.Fatalf("links: %v", err)
	}
	if links != nil {
		log.Printf("links on, sensors at least %.0f degrees apart", links.minAngleDeg)
	}

	metrics.Serve(env("METRICS_ADDR", ":9100"))

	kafka, err := kgo.NewClient(
		kgo.SeedBrokers(strings.Split(env("KAFKA_BROKERS", "localhost:29092"), ",")...),
		kgo.ConsumerGroup(group),
		kgo.ConsumeTopics(rawTopic),
		kgo.DisableAutoCommit(),
	)
	if err != nil {
		log.Fatalf("kafka: %v", err)
	}
	defer kafka.Close()

	db, err := pgxpool.New(ctx, env("DATABASE_URL", "postgres://neuro:neuro@localhost:5432/neuro"))
	if err != nil {
		log.Fatalf("postgres: %v", err)
	}
	defer db.Close()

	p := &processor{kafka: kafka, db: db, sessions: map[string]*session{}, links: links}
	log.Printf("processor started, group %q reading %s", group, rawTopic)
	if err := p.run(ctx); err != nil {
		log.Fatalf("processor stopped: %v", err)
	}
	log.Print("processor stopped")
}
