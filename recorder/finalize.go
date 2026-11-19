package main

// Writing a session's EDF file once it has ended: claim the session in
// Postgres, merge its chunks, write the file, upload it.

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"sort"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"github.com/neurohalo/neurohalo/internal/recording"
)

// The finalizer's Prometheus metrics.
var (
	// filesWritten counts finalize attempts by result: ready, failed (will not
	// be tried again) or retry. Raised by: recorder.finalizeLoop.
	filesWritten = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "recorder_files_total",
		Help: "EDF files written, by result.",
	}, []string{"result"})
	// fileSeconds times one file, from the first chunk read to the upload.
	// Observed by: recorder.finalizeLoop.
	fileSeconds = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "recorder_file_seconds",
		Help:    "Time to merge a session's chunks and store its EDF file.",
		Buckets: prometheus.ExponentialBuckets(0.05, 2, 12), // 50 ms to about 100 s
	})
)

const (
	finalizeEvery = 2 * time.Second
	staleWriting  = 10 * time.Minute // a claim this old belongs to a recorder that died
)

// permanentError is a failure that trying again cannot fix, such as a
// sample rate EDF cannot hold. The recording is marked failed.
// Created by: permanent. Checked by: recorder.finalizeLoop.
type permanentError struct{ error }

// permanent marks err as one that trying again cannot fix.
func permanent(err error) error { return permanentError{err} }

// finalizeLoop writes the file of every session that is ready for it, every
// 2 seconds, one session at a time. A failure that may pass (the store is
// down) puts the session back, to be tried when it has been quiet for
// idleAfter; any other failure marks it failed with the reason.
func (r *recorder) finalizeLoop(ctx context.Context) {
	tick := time.NewTicker(finalizeEvery)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		ids, err := r.claim(ctx)
		if err != nil {
			if ctx.Err() == nil {
				log.Printf("finalize: claim: %v", err)
			}
			continue
		}
		for _, id := range ids {
			start := time.Now()
			info, err := r.finalize(ctx, id)
			fileSeconds.Observe(time.Since(start).Seconds())
			result, status, message := "ready", "ready", ""
			var perm permanentError
			switch {
			case err == nil:
				log.Printf("session %s: wrote %s, %.1f s of signal, %d batches (%d duplicates dropped), %d bytes",
					id, info.key, info.seconds, info.batches, info.duplicates, info.bytes)
			case errors.As(err, &perm):
				result, status, message = "failed", "failed", err.Error()
				log.Printf("session %s: cannot write its file: %v", id, err)
			default:
				result, status, message = "retry", "recording", err.Error()
				log.Printf("session %s: writing its file failed, will try again: %v", id, err)
			}
			filesWritten.WithLabelValues(result).Inc()
			r.finish(ctx, id, status, message, info)
		}
	}
}

// claim marks the sessions that are ready to be written as "writing" and
// returns their IDs. A session is ready when the gateway saw it end and its
// last batch is in a chunk, or when nothing new has arrived for idleAfter
// (the stream broke, or a retry is due). A claim left behind by a recorder
// that died is taken over after staleWriting. The UPDATE makes the claim
// atomic, so two recorders never write the same file.
func (r *recorder) claim(ctx context.Context) ([]string, error) {
	qctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	rows, err := r.db.Query(qctx, `
		UPDATE recordings r SET status = 'writing', updated_at = now()
		FROM sessions s
		WHERE r.session_id = s.id AND (
			(r.status = 'recording' AND r.error = '' AND s.status = 'ended' AND r.saved_seq >= s.last_seq)
			OR (r.status = 'recording' AND r.updated_at < now() - make_interval(secs => $1))
			OR (r.status = 'writing' AND r.updated_at < now() - make_interval(secs => $2)))
		RETURNING r.session_id`,
		r.idleAfter.Seconds(), staleWriting.Seconds())
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// fileInfo describes a written file.
// Returned by: recorder.finalize. Used by: finalizeLoop (log) and finish (stored).
type fileInfo struct {
	key                 string
	bytes               int64
	seconds             float64
	batches, duplicates int
}

// finish stores the outcome of a claim. The row is only changed while it is
// still "writing": if new batches arrived meanwhile, saveProgress already
// put it back to "recording" and the file will be written again.
func (r *recorder) finish(ctx context.Context, id, status, message string, info fileInfo) {
	err := retry(ctx, "session "+id+": finish", func(ctx context.Context) error {
		qctx, cancel := context.WithTimeout(ctx, dbTimeout)
		defer cancel()
		_, err := r.db.Exec(qctx, `
			UPDATE recordings SET status = $2, error = $3, updated_at = now(),
				object_key = CASE WHEN $2 = 'ready' THEN $4 ELSE object_key END,
				bytes      = CASE WHEN $2 = 'ready' THEN $5 ELSE bytes END,
				seconds    = CASE WHEN $2 = 'ready' THEN $6 ELSE seconds END
			WHERE session_id = $1 AND status = 'writing'`,
			id, status, message, info.key, info.bytes, info.seconds)
		return err
	})
	if err != nil && ctx.Err() == nil {
		log.Printf("session %s: cannot store the result: %v", id, err)
	}
}

// finalize writes one session's EDF file: it lists the session's chunks in
// seq order, merges them into a temporary file of samples, writes the EDF
// from that into a second temporary file, and uploads it. Both passes read
// from disk, so a long session never has to fit in memory.
func (r *recorder) finalize(ctx context.Context, id string) (fileInfo, error) {
	var info fileInfo
	var rate float64
	var names []string
	var started time.Time
	err := r.db.QueryRow(ctx, `SELECT sample_rate_hz, channel_names, started_at FROM sessions WHERE id = $1`, id).
		Scan(&rate, &names, &started)
	if err != nil {
		return info, fmt.Errorf("session details: %w", err)
	}

	var keys []string
	lctx, stopList := context.WithCancel(ctx) // stops the listing goroutine on an early return
	defer stopList()
	for obj := range r.store.ListObjects(lctx, r.bucket, minio.ListObjectsOptions{Prefix: recording.SpoolDir(id), Recursive: true}) {
		if obj.Err != nil {
			return info, fmt.Errorf("listing chunks: %w", obj.Err)
		}
		keys = append(keys, obj.Key)
	}
	if len(keys) == 0 {
		return info, permanent(errors.New("no chunks stored"))
	}
	sort.Strings(keys) // the names start with the first seq, zero-padded

	samples, err := os.CreateTemp("", "samples-*.f32")
	if err != nil {
		return info, err
	}
	defer os.Remove(samples.Name())
	defer samples.Close()
	sampleOut := bufio.NewWriterSize(samples, 1<<20)
	m := newMerger(rate, len(names), sampleOut)
	for _, key := range keys {
		if err := r.readChunkObject(ctx, key, m); err != nil {
			return info, err
		}
	}
	if err := sampleOut.Flush(); err != nil {
		return info, err
	}
	if m.malformed > 0 {
		log.Printf("session %s: %d batches with the wrong number of values were left out", id, m.malformed)
	}
	if _, err := samples.Seek(0, io.SeekStart); err != nil {
		return info, err
	}

	file, err := os.CreateTemp("", "recording-*.edf")
	if err != nil {
		return info, err
	}
	defer os.Remove(file.Name())
	defer file.Close()
	err = writeEDF(file, &edfFile{
		sessionID: id, start: started, rate: rate, names: names,
		samples: m.samples, min: m.min, max: m.max, annotations: m.annotations,
	}, bufio.NewReaderSize(samples, 1<<20))
	if err != nil {
		return info, err
	}
	size, err := file.Seek(0, io.SeekCurrent)
	if err != nil {
		return info, err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return info, err
	}
	info = fileInfo{key: recording.FileKey(id), bytes: size, seconds: float64(m.samples) / rate, batches: m.batches, duplicates: m.duplicates}
	_, err = r.store.PutObject(ctx, r.bucket, info.key, file, size, minio.PutObjectOptions{ContentType: "application/octet-stream"})
	return info, err
}

// readChunkObject downloads one chunk and feeds its batches to the merger.
func (r *recorder) readChunkObject(ctx context.Context, key string, m *merger) error {
	obj, err := r.store.GetObject(ctx, r.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return fmt.Errorf("reading %s: %w", key, err)
	}
	defer obj.Close()
	if err := readChunk(bufio.NewReaderSize(obj, 1<<20), m.add); err != nil {
		return fmt.Errorf("reading %s: %w", key, err)
	}
	return nil
}
