package main

import (
	"context"
	"errors"
	"log"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/twmb/franz-go/pkg/kmsg"
	"google.golang.org/protobuf/proto"

	"github.com/neurohalo/neurohalo/internal/eeg"

	neurov1 "github.com/neurohalo/neurohalo/gen/neuro/v1"
)

const (
	featuresTopic = "eeg.features"
	eventsTopic   = "eeg.events"
	storeGroup    = "api-store"

	rollupSeconds  = 10
	idleFlushAfter = 5 * time.Second  // write a bucket that has stopped receiving frames
	sessionTTL     = 10 * time.Minute // forget cached session details after this long unused
)

// sessionMeta is what the store needs to place a frame in time.
// Created by: store.session (read from Postgres).
// Used by: store.handleFrame, store.handleEvent.
type sessionMeta struct {
	startedAt time.Time
	rate      float64
	names     []string
	lastUsed  time.Time
}

// bucket collects one session's frames for one 10-second rollup until it is
// written. It remembers the Kafka offset of its first frame, so offsets are
// never committed past a frame whose rollup is not in Postgres yet.
// Created by: store.handleFrame.
// Used by: add (its method), store.flush, store.commit, store.dropPartitions.
type bucket struct {
	sessionID   string
	index       int64 // 10-second buckets since the session started
	start       time.Time
	partition   int32
	firstOffset int64

	sum       []float64       // channels x bands
	notOK     []int           // per channel, frames whose quality was not OK
	seen      map[uint64]bool // window indexes already counted, so a repeat counts once
	lastAdded time.Time
}

// add counts one frame into the bucket, unless its window was counted already.
func (b *bucket) add(f *neurov1.FeatureFrame) {
	if b.seen[f.WindowIndex] {
		return
	}
	b.seen[f.WindowIndex] = true
	b.lastAdded = time.Now()
	for i, p := range f.BandPower {
		b.sum[i] += float64(p)
	}
	for c, q := range f.Quality {
		if q != neurov1.Quality_QUALITY_OK {
			b.notOK[c]++
		}
	}
}

// store is the api-store consumer: it writes events and 10-second rollups to
// Postgres, and commits Kafka offsets only for what is written.
// Created by: main.
// Used by: run, handle, handleEvent, handleFrame, session, flush, flushIdle,
// writeEvents, commit, dropPartitions (its methods).
type store struct {
	kafka *kgo.Client
	db    *pgxpool.Pool

	meta   map[string]*sessionMeta // nil value: session is not in Postgres, skip it
	open   map[string]*bucket      // by session ID
	events []*neurov1.Event        // read this poll, not written yet

	// Offset after the last record handled, per topic and partition.
	next  map[string]map[int32]int64
	dirty bool // something to commit
}

// newStore returns a store with empty state. main attaches the Kafka client.
func newStore(db *pgxpool.Pool) *store {
	return &store{
		db:   db,
		meta: map[string]*sessionMeta{},
		open: map[string]*bucket{},
		next: map[string]map[int32]int64{featuresTopic: {}, eventsTopic: {}},
	}
}

// retry runs fn until it succeeds, the error is not worth retrying, or ctx
// ends. Connection problems are retried with backoff, which also holds the
// Kafka offsets while Postgres is down. Errors from Postgres itself (bad data,
// broken constraints) and "no rows" are returned at once.
func retry(ctx context.Context, what string, fn func(context.Context) error) error {
	backoff := 500 * time.Millisecond
	for {
		qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err := fn(qctx)
		cancel()
		var pgErr *pgconn.PgError
		if err == nil || errors.Is(err, pgx.ErrNoRows) || errors.As(err, &pgErr) || ctx.Err() != nil {
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

// session returns the cached details of a session, reading them from Postgres
// the first time. It returns nil for a session Postgres does not know.
func (s *store) session(ctx context.Context, id string) (*sessionMeta, error) {
	m, known := s.meta[id]
	if !known {
		m = &sessionMeta{}
		err := retry(ctx, "load session "+id, func(ctx context.Context) error {
			return s.db.QueryRow(ctx,
				`SELECT started_at, sample_rate_hz, channel_names FROM sessions WHERE id = $1`, id).
				Scan(&m.startedAt, &m.rate, &m.names)
		})
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			log.Printf("session %s: not in Postgres, its frames and events are skipped", id)
			m = nil
		case err != nil:
			return nil, err
		}
		s.meta[id] = m
	}
	if m != nil {
		m.lastUsed = time.Now()
	}
	return m, nil
}

// handle routes one record to the event or frame handler and remembers its
// offset for the next commit.
func (s *store) handle(ctx context.Context, rec *kgo.Record) error {
	s.next[rec.Topic][rec.Partition] = rec.Offset + 1
	s.dirty = true
	if rec.Topic == eventsTopic {
		return s.handleEvent(ctx, rec)
	}
	return s.handleFrame(ctx, rec)
}

// handleEvent queues one event to be written at the end of this poll.
func (s *store) handleEvent(ctx context.Context, rec *kgo.Record) error {
	var e neurov1.Event
	if err := proto.Unmarshal(rec.Value, &e); err != nil {
		log.Printf("%s/%d@%d: not an Event, skipped: %v", rec.Topic, rec.Partition, rec.Offset, err)
		return nil
	}
	m, err := s.session(ctx, e.SessionId)
	if err != nil || m == nil {
		return err
	}
	s.events = append(s.events, &e)
	return nil
}

// handleFrame adds one frame to its session's open bucket. A frame for a
// later bucket first writes the open one. A frame for an earlier bucket (a
// processor recomputing after a restart) is skipped, because that bucket is
// already written.
func (s *store) handleFrame(ctx context.Context, rec *kgo.Record) error {
	var f neurov1.FeatureFrame
	if err := proto.Unmarshal(rec.Value, &f); err != nil {
		log.Printf("%s/%d@%d: not a FeatureFrame, skipped: %v", rec.Topic, rec.Partition, rec.Offset, err)
		return nil
	}
	m, err := s.session(ctx, f.SessionId)
	if err != nil || m == nil {
		return err
	}
	if len(f.Quality) != len(m.names) || len(f.BandPower) != len(m.names)*eeg.NumBands {
		return nil
	}

	// Window i ends i x hop samples into the recording.
	endSeconds := float64(f.WindowIndex*uint64(eeg.HopSamples(m.rate))) / m.rate
	index := int64(endSeconds) / rollupSeconds

	b := s.open[f.SessionId]
	if b != nil && index > b.index {
		if err := s.flush(ctx, b); err != nil {
			return err
		}
		b = nil
	}
	if b != nil && index < b.index {
		return nil
	}
	if b == nil {
		b = &bucket{
			sessionID:   f.SessionId,
			index:       index,
			start:       m.startedAt.Add(time.Duration(index*rollupSeconds) * time.Second),
			partition:   rec.Partition,
			firstOffset: rec.Offset,
			sum:         make([]float64, len(f.BandPower)),
			notOK:       make([]int, len(f.Quality)),
			seen:        map[uint64]bool{},
		}
		s.open[f.SessionId] = b
	}
	b.add(&f)
	return nil
}

// flush writes one bucket's averages to Postgres and forgets the bucket. The
// write is idempotent: a second write of the same bucket replaces the row only
// if it averaged at least as many frames.
func (s *store) flush(ctx context.Context, b *bucket) error {
	frames := len(b.seen)
	power := make([]float32, len(b.sum))
	for i, v := range b.sum {
		power[i] = float32(v / float64(frames))
	}
	names := s.meta[b.sessionID].names
	bad := []string{}
	for c, n := range b.notOK {
		if 2*n > frames {
			bad = append(bad, names[c])
		}
	}
	err := retry(ctx, "write rollup "+b.sessionID, func(ctx context.Context) error {
		_, err := s.db.Exec(ctx, `
			INSERT INTO rollups (session_id, bucket_start, band_power, bad_channels, frames)
			VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (session_id, bucket_start) DO UPDATE
			SET band_power = EXCLUDED.band_power, bad_channels = EXCLUDED.bad_channels, frames = EXCLUDED.frames
			WHERE rollups.frames <= EXCLUDED.frames`,
			b.sessionID, b.start, power, bad, frames)
		return err
	})
	if err != nil {
		return err
	}
	delete(s.open, b.sessionID)
	s.dirty = true
	return nil
}

// flushIdle writes buckets that have had no frame for a while, such as the
// last bucket of a session that ended. It also forgets session details that
// have not been used for sessionTTL.
func (s *store) flushIdle(ctx context.Context) error {
	for _, b := range s.open {
		if time.Since(b.lastAdded) > idleFlushAfter {
			if err := s.flush(ctx, b); err != nil {
				return err
			}
		}
	}
	for id, m := range s.meta {
		if _, open := s.open[id]; !open && (m == nil || time.Since(m.lastUsed) > sessionTTL) {
			delete(s.meta, id)
		}
	}
	return nil
}

// writeEvents writes this poll's events in one round trip. A repeated event
// changes nothing, thanks to the UNIQUE constraint and ON CONFLICT DO NOTHING.
func (s *store) writeEvents(ctx context.Context) error {
	if len(s.events) == 0 {
		return nil
	}
	err := retry(ctx, "write events", func(ctx context.Context) error {
		var batch pgx.Batch
		for _, e := range s.events {
			details := e.DetailsJson
			if details == "" {
				details = "{}"
			}
			batch.Queue(`
				INSERT INTO events (session_id, type, channel, at, details)
				VALUES ($1, $2, $3, $4, $5::jsonb)
				ON CONFLICT DO NOTHING`,
				e.SessionId, e.Type, e.Channel, time.Unix(0, e.TimeUnixNanos), details)
		}
		return s.db.SendBatch(ctx, &batch).Close()
	})
	if err != nil {
		return err
	}
	s.events = s.events[:0]
	return nil
}

// commit tells Kafka how far this consumer has stored. On each partition of
// eeg.features it stops at the first frame of any bucket not written yet, so
// after a crash those frames are read again and the bucket is rebuilt whole.
func (s *store) commit(ctx context.Context) {
	if !s.dirty {
		return
	}
	offsets := map[string]map[int32]kgo.EpochOffset{}
	for topic, parts := range s.next {
		offsets[topic] = map[int32]kgo.EpochOffset{}
		for p, next := range parts {
			offsets[topic][p] = kgo.EpochOffset{Epoch: -1, Offset: next}
		}
	}
	for _, b := range s.open {
		if cur, ok := offsets[featuresTopic][b.partition]; ok && b.firstOffset < cur.Offset {
			offsets[featuresTopic][b.partition] = kgo.EpochOffset{Epoch: -1, Offset: b.firstOffset}
		}
	}
	s.kafka.CommitOffsetsSync(ctx, offsets,
		func(_ *kgo.Client, _ *kmsg.OffsetCommitRequest, _ *kmsg.OffsetCommitResponse, err error) {
			if err != nil {
				log.Printf("commit: %v", err)
			}
		})
	s.dirty = false
}

// dropPartitions forgets the state tied to partitions this consumer no longer
// owns. Whoever owns them next starts from the last commit and rebuilds any
// unwritten bucket. Registered with Kafka in main for revoked and lost partitions.
func (s *store) dropPartitions(_ context.Context, _ *kgo.Client, gone map[string][]int32) {
	for topic, parts := range gone {
		for _, p := range parts {
			delete(s.next[topic], p)
			if topic != featuresTopic {
				continue
			}
			for id, b := range s.open {
				if b.partition == p {
					delete(s.open, id)
				}
			}
		}
	}
}

// run is the consumer loop: fetch, handle each record, write events, write
// idle buckets, commit, repeat. It polls at least once a second so idle
// buckets are written on time. Kafka rebalances wait until a pass is finished.
func (s *store) run(ctx context.Context) error {
	for {
		pollCtx, cancel := context.WithTimeout(ctx, time.Second)
		fetches := s.kafka.PollFetches(pollCtx)
		cancel()
		if ctx.Err() != nil || fetches.IsClientClosed() {
			return nil
		}
		fetches.EachError(func(topic string, p int32, err error) {
			if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
				log.Printf("fetch %s/%d: %v", topic, p, err)
			}
		})

		var err error
		fetches.EachRecord(func(rec *kgo.Record) {
			if err == nil {
				err = s.handle(ctx, rec)
			}
		})
		if err == nil {
			err = s.writeEvents(ctx)
		}
		if err == nil {
			err = s.flushIdle(ctx)
		}
		if err == nil {
			s.commit(ctx)
		}
		s.kafka.AllowRebalance()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
	}
}
