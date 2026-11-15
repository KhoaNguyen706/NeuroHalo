package main

// Chunks: how batches wait in the object store until their session ends, and
// how they are merged back into one stream of samples.

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"

	"google.golang.org/protobuf/proto"

	"github.com/neurohalo/neurohalo/internal/recording"

	neurov1 "github.com/neurohalo/neurohalo/gen/neuro/v1"
)

const maxBatchBytes = 64 << 20 // far above a real batch; guards against a damaged chunk

// chunk holds one session's batches that are not uploaded yet, each as the
// bytes Kafka delivered: a uvarint length, then the encoded SampleBatch.
// Created by: recorder.handle. Uploaded by: recorder.flush.
type chunk struct {
	firstSeq, lastSeq uint64
	batches           int
	buf               bytes.Buffer
}

// add appends one encoded batch. Batches arrive in seq order.
func (c *chunk) add(seq uint64, value []byte) {
	if c.batches == 0 {
		c.firstSeq = seq
	}
	c.lastSeq = seq
	c.batches++
	c.buf.Write(binary.AppendUvarint(nil, uint64(len(value))))
	c.buf.Write(value)
}

// chunkKey names a chunk by the first and last seq it holds, zero-padded so
// the names sort in seq order. Two chunks never share a name unless they hold
// the same batches, so an upload never replaces different data.
func chunkKey(sessionID string, c *chunk) string {
	return fmt.Sprintf("%s%020d-%020d.bin", recording.SpoolDir(sessionID), c.firstSeq, c.lastSeq)
}

// readChunk decodes the batches of one chunk and hands them to fn in order.
func readChunk(r *bufio.Reader, fn func(*neurov1.SampleBatch) error) error {
	for {
		n, err := binary.ReadUvarint(r)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if n > maxBatchBytes {
			return fmt.Errorf("chunk holds a %d-byte batch, more than the %d allowed", n, maxBatchBytes)
		}
		value := make([]byte, n)
		if _, err := io.ReadFull(r, value); err != nil {
			return fmt.Errorf("chunk ends inside a batch: %w", err)
		}
		var b neurov1.SampleBatch
		if err := proto.Unmarshal(value, &b); err != nil {
			return fmt.Errorf("chunk holds something that is not a SampleBatch: %w", err)
		}
		if err := fn(&b); err != nil {
			return err
		}
	}
}

// merger turns a session's batches, read chunk after chunk in seq order,
// into one gap-free run of samples: float32 rows written to out, plus each
// sensor's range and the annotations for the EDF file.
//   - A batch at or below the last seq taken is a duplicate (Kafka delivers
//     at least once, and a client resends after a reconnect) and is skipped.
//   - Samples missing between two batches are written as 0 µV and get a
//     "no data" annotation, so every later sample keeps its true time.
//   - A marker becomes an annotation at its own sample.
//
// Created by: newMerger (called from recorder.finalize).
// Used by: recorder.finalize, which reads the counts and ranges afterwards.
type merger struct {
	rate     float64
	channels int
	out      io.Writer

	started bool
	lastSeq uint64
	origin  uint64 // the sample index of the session's first sample
	next    uint64 // the sample index the next row written will have

	samples     int64 // rows written, including the 0 µV ones
	min, max    []float64
	annotations []annotation
	batches     int // batches taken
	duplicates  int // batches skipped as already taken
	malformed   int // batches whose value count does not match the sensors

	row []byte
}

// newMerger starts an empty merge for a session's sample rate and sensor count.
func newMerger(rate float64, channels int, out io.Writer) *merger {
	m := &merger{rate: rate, channels: channels, out: out,
		min: make([]float64, channels), max: make([]float64, channels), row: make([]byte, channels*4)}
	for c := range channels {
		m.min[c], m.max[c] = math.Inf(1), math.Inf(-1)
	}
	return m
}

// at is a sample index as seconds into the file.
func (m *merger) at(sample uint64) float64 {
	return float64(sample-m.origin) / m.rate
}

// writeRow writes one row of samples and widens each sensor's range to hold
// it. A value that is not a number is written as 0.
func (m *merger) writeRow(values []float32) error {
	for c, v := range values {
		if math.IsNaN(float64(v)) || math.IsInf(float64(v), 0) {
			v = 0
		}
		m.min[c], m.max[c] = min(m.min[c], float64(v)), max(m.max[c], float64(v))
		binary.LittleEndian.PutUint32(m.row[c*4:], math.Float32bits(v))
	}
	m.samples++
	_, err := m.out.Write(m.row)
	return err
}

// add takes one batch (see merger for the rules).
func (m *merger) add(b *neurov1.SampleBatch) error {
	if len(b.Values) != int(b.NumSamples)*m.channels {
		m.malformed++
		return nil
	}
	if m.started && b.Seq <= m.lastSeq {
		m.duplicates++
		return nil
	}
	if !m.started {
		m.started, m.origin, m.next = true, b.FirstSampleIndex, b.FirstSampleIndex
	}
	m.lastSeq = b.Seq
	m.batches++

	first, end := b.FirstSampleIndex, b.FirstSampleIndex+uint64(b.NumSamples)
	if end <= m.next {
		return nil // every sample is already written
	}
	if first > m.next {
		m.annotations = append(m.annotations, annotation{onset: m.at(m.next), duration: float64(first-m.next) / m.rate, text: noDataText})
		zeros := make([]float32, m.channels)
		for ; m.next < first; m.next++ {
			if err := m.writeRow(zeros); err != nil {
				return err
			}
		}
	}
	for _, mk := range b.Markers {
		if mk.SampleIndex >= m.next && mk.SampleIndex < end {
			m.annotations = append(m.annotations, annotation{onset: m.at(mk.SampleIndex), duration: mk.DurationSeconds, text: mk.Label})
		}
	}
	for i := m.next - first; i < uint64(b.NumSamples); i++ {
		if err := m.writeRow(b.Values[i*uint64(m.channels) : (i+1)*uint64(m.channels)]); err != nil {
			return err
		}
	}
	m.next = end
	return nil
}
