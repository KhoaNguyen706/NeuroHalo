package main

import (
	"math"

	"github.com/neurohalo/neurohalo/internal/eeg"

	neurov1 "github.com/neurohalo/neurohalo/gen/neuro/v1"
)

// An epoch is the signal around one marker: band power in 1-second windows
// that step every quarter second, from windows that end 1 second before the
// marker to windows that end 4 seconds after it. The windows before the
// marker are what "after" is compared with.
const (
	epochWindowSeconds = 1.0
	epochStepSeconds   = 0.25
	epochPreSteps      = 5  // windows ending 1 s before the marker up to exactly at it
	epochPostSteps     = 16 // windows ending 0.25 s to 4 s after it
	epochSteps         = epochPreSteps + epochPostSteps
)

// pendingEpoch is one epoch being filled: its marker, how many of its windows
// are computed, and their results so far.
// Created by: session.startEpochs.
// Used by: session.stepEpochs, session.epochStepPower, session.finishEpoch.
type pendingEpoch struct {
	label  string
	sample uint64    // the marker's position in the session
	next   int       // windows computed so far
	power  []float32 // epochSteps x channels x bands
	bad    []bool    // per channel: flat or noisy in any window so far
}

// initEpochs works out the epoch window and step in samples for this
// session's rate and allocates what computing them needs. Epochs stay off
// (epochWin 0) when the windows before a marker would not fit in the
// 2-second buffer, which cannot happen at the constants above.
// Used by: newSession.
func (s *session) initEpochs() {
	win := int(math.Round(epochWindowSeconds * s.rate))
	step := int(math.Round(epochStepSeconds * s.rate))
	if step < 1 || win+(epochPreSteps-1)*step > s.win {
		return
	}
	s.epochWin, s.epochStep = win, step
	s.epochAnalyzer = eeg.NewAnalyzer(win, s.rate)
	s.epochScratch = make([]float32, win)
}

// startEpochs opens an epoch for each marker of the batch being added. The
// windows are computed later, sample by sample, in stepEpochs.
// Used by: add.
func (s *session) startEpochs(markers []marker) {
	if s.epochWin == 0 {
		return
	}
	for _, m := range markers {
		s.pending = append(s.pending, &pendingEpoch{
			label:  m.label,
			sample: m.sample,
			power:  make([]float32, epochSteps*s.channels*eeg.NumBands),
			bad:    make([]bool, s.channels),
		})
	}
}

// stepEpochs is called after each sample row is copied into the buffer, with
// that row's position in the session. For each open epoch it computes the
// windows that are due and returns the epochs this row completed.
//
// At the marker's own sample, the windows that end before and at the marker
// are computed together, read back out of the 2-second buffer. An epoch whose
// marker comes before the buffer is full (the first 2 seconds of a stream, or
// right after a gap) has no "before" to compare with and is dropped. After
// the marker, one window is computed every epochStep samples.
// Used by: add.
func (s *session) stepEpochs(sampleIndex uint64) []*neurov1.Epoch {
	if len(s.pending) == 0 {
		return nil
	}
	var done []*neurov1.Epoch
	kept := s.pending[:0]
	for _, e := range s.pending {
		switch {
		case sampleIndex < e.sample:
			// The marker's sample is later in this batch.
		case sampleIndex == e.sample:
			if s.filled < s.win {
				continue
			}
			for i := epochPreSteps - 1; i >= 0; i-- {
				s.epochStepPower(e, i*s.epochStep)
			}
		case e.next == 0:
			continue // its marker sample was never seen; cannot happen with a valid batch
		case (sampleIndex-e.sample)%uint64(s.epochStep) == 0:
			s.epochStepPower(e, 0)
		}
		if e.next == epochSteps {
			done = append(done, s.finishEpoch(e))
			continue
		}
		kept = append(kept, e)
	}
	// Clear the tail so dropped epochs can be freed.
	for i := len(kept); i < len(s.pending); i++ {
		s.pending[i] = nil
	}
	s.pending = kept
	return done
}

// epochStepPower computes the epoch's next window for every sensor: the
// epochWin samples that end `back` rows before the newest row in the buffer.
// A sensor that is flat or noisy in the window is marked bad for the epoch.
// Used by: stepEpochs.
func (s *session) epochStepPower(e *pendingEpoch, back int) {
	// s.pos is where the next row will go, so the newest row is at pos-1.
	first := s.pos - back - s.epochWin + 2*s.win
	for c := 0; c < s.channels; c++ {
		for i := 0; i < s.epochWin; i++ {
			s.epochScratch[i] = s.ring[((first+i)%s.win)*s.channels+c]
		}
		at := (e.next*s.channels + c) * eeg.NumBands
		if q := s.epochAnalyzer.Power(s.epochScratch, e.power[at:at+eeg.NumBands]); q != neurov1.Quality_QUALITY_OK {
			e.bad[c] = true
		}
	}
	e.next++
}

// finishEpoch turns a filled epoch into the message for eeg.epochs. Its time
// is the marker's time on the recording's clock, so an epoch computed twice
// is exactly equal and Postgres keeps one copy.
// Used by: stepEpochs.
func (s *session) finishEpoch(e *pendingEpoch) *neurov1.Epoch {
	return &neurov1.Epoch{
		SessionId:     s.id,
		Label:         e.label,
		MarkerSample:  e.sample,
		TimeUnixNanos: s.sampleTime(e.sample).UnixNano(),
		Steps:         epochSteps,
		PreSteps:      epochPreSteps,
		StepSeconds:   float32(s.epochStep) / float32(s.rate),
		WindowSeconds: float32(s.epochWin) / float32(s.rate),
		BandPower:     e.power,
		Bad:           e.bad,
	}
}
