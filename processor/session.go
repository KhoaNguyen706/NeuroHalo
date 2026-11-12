package main

import (
	"encoding/json"
	"math"
	"time"

	"github.com/neurohalo/neurohalo/internal/eeg"

	neurov1 "github.com/neurohalo/neurohalo/gen/neuro/v1"
)

const (
	badAfterWindows       = 3   // flat or noisy this many windows in a row -> channel_bad
	recoveredAfterWindows = 3   // OK this many windows in a row -> channel_recovered
	alphaRiseRatio        = 1.5 // times the session's running average
	alphaRiseWindows      = 4
	alphaWarmupWindows    = 10 // windows of history needed before alpha_rise can fire
)

// session is everything the processor remembers about one stream. None of it
// is saved: after a restart it starts empty and refills within 2 seconds.
// Created by: newSession (called from processor.loadSession).
// Used by: add, compute, detect, windowTime (its methods) and processor.handle.
type session struct {
	id        string
	names     []string
	channels  int
	rate      float64
	startedAt time.Time
	win, hop  int
	analyzer  *eeg.Analyzer
	back      []int // indexes of back-of-head sensors

	haveSeq bool
	lastSeq uint64

	ring    []float32 // win rows x channels, oldest row at pos once full
	pos     int
	filled  int
	scratch []float32 // one sensor's window, in time order

	// Correlation links; linkPairs is nil when LINKS_ENABLED is off.
	linkPairs   [][2]int  // pairs far enough apart, from linkConfig.allowedPairs
	linkWindows []float32 // every sensor's window, channels x win, in time order
	linkScratch []float64 // centered copy of linkWindows, used by computeLinks

	// Per sensor: how many windows in a row were not OK, the latest quality and
	// how many windows in a row had it, whether the sensor is marked bad, and
	// the reason it was last marked bad.
	badRun  []int
	lastQ   []neurov1.Quality
	sameRun []int
	bad     []bool
	badQ    []neurov1.Quality

	alphaSum   float64 // running average of back-of-head alpha
	alphaCount int
	alphaAbove int

	// Artifacts (artifacts.go): the sensors above the eyes, scratch space for
	// groupSwing, whether the last window held an eye movement, and how many
	// windows in a row showed muscle activity.
	frontPole  []int
	eyeScratch []float64
	eyeActive  bool
	muscleRun  int

	// Epochs (epochs.go): the window and step sizes in samples, the analyzer
	// for that window, scratch space for one sensor's window, and the epochs
	// whose marker has been seen but whose last window is still to come.
	// epochWin is 0 when the 2-second buffer is too short for them.
	epochWin, epochStep int
	epochAnalyzer       *eeg.Analyzer
	epochScratch        []float32
	pending             []*pendingEpoch

	lastSeen time.Time
}

// newSession builds empty state for one stream: a 2-second buffer per sensor,
// an analyzer for that window size, and the list of back-of-head sensors.
// With links (nil when they are off) it also lists the sensor pairs to correlate.
func newSession(id string, sampleRateHz float64, names []string, startedAt time.Time, links *linkConfig) *session {
	win := eeg.WindowSamples(sampleRateHz)
	n := len(names)
	s := &session{
		id:        id,
		names:     names,
		channels:  n,
		rate:      sampleRateHz,
		startedAt: startedAt,
		win:       win,
		hop:       eeg.HopSamples(sampleRateHz),
		analyzer:  eeg.NewAnalyzer(win, sampleRateHz),
		ring:      make([]float32, win*n),
		scratch:   make([]float32, win),
		badRun:    make([]int, n),
		lastQ:     make([]neurov1.Quality, n),
		sameRun:   make([]int, n),
		bad:       make([]bool, n),
		badQ:      make([]neurov1.Quality, n),

		frontPole:  frontPoleSensors(names),
		eyeScratch: make([]float64, win),
	}
	s.initEpochs()
	if links != nil {
		s.linkPairs = links.allowedPairs(names)
		s.linkWindows = make([]float32, win*n)
		s.linkScratch = make([]float64, win*n)
	}
	for i, name := range names {
		if eeg.BackOfHead(name) {
			s.back = append(s.back, i)
		}
	}
	return s
}

// batchResult is what one batch produced.
// Returned by: session.add.
// Used by: processor.handle, which counts and publishes its contents.
type batchResult struct {
	duplicate bool
	gap       bool
	frames    []*neurov1.FeatureFrame
	events    []*neurov1.Event
	epochs    []*neurov1.Epoch
}

// add feeds one batch in and returns whatever frames, events and epochs it
// completes. A batch at or below the last seq is dropped as a duplicate. A
// jump in seq clears the buffers, drops the unfinished epochs and is reported
// as a gap.
func (s *session) add(b *neurov1.SampleBatch) batchResult {
	var res batchResult
	s.lastSeen = time.Now()

	if s.haveSeq && b.Seq <= s.lastSeq {
		res.duplicate = true
		return res
	}
	if s.haveSeq && b.Seq != s.lastSeq+1 {
		res.gap = true
		s.pos, s.filled = 0, 0
		s.pending = nil
	}
	s.haveSeq, s.lastSeq = true, b.Seq
	markers := mergeMarkers(b)
	res.events = s.markerEvents(markers)
	s.startEpochs(markers)

	for row := 0; row < int(b.NumSamples); row++ {
		copy(s.ring[s.pos*s.channels:], b.Values[row*s.channels:(row+1)*s.channels])
		s.pos = (s.pos + 1) % s.win
		if s.filled < s.win {
			s.filled++
		}
		res.epochs = append(res.epochs, s.stepEpochs(b.FirstSampleIndex+uint64(row))...)
		// Recompute on fixed sample positions, so a window's index is the same
		// no matter which processor computes it or when it restarted.
		sampleCount := b.FirstSampleIndex + uint64(row) + 1
		if s.filled == s.win && sampleCount%uint64(s.hop) == 0 {
			began := time.Now()
			frame := s.compute(sampleCount/uint64(s.hop), b.SentTimeUnixNanos)
			windowComputeSeconds.Observe(time.Since(began).Seconds())
			res.frames = append(res.frames, frame)
			eye, eyeSwing := s.eyeMovement()
			res.events = append(res.events, s.detect(frame, eye, eyeSwing)...)
		}
	}
	return res
}

// compute builds one FeatureFrame from the current 2-second buffer: band
// power and quality for every sensor, plus the correlation links when they are on.
func (s *session) compute(windowIndex uint64, sentTime int64) *neurov1.FeatureFrame {
	frame := &neurov1.FeatureFrame{
		SessionId:               s.id,
		WindowIndex:             windowIndex,
		NewestSentTimeUnixNanos: sentTime,
		BandPower:               make([]float32, s.channels*eeg.NumBands),
		Quality:                 make([]neurov1.Quality, s.channels),
	}
	for c := 0; c < s.channels; c++ {
		for i := 0; i < s.win; i++ {
			s.scratch[i] = s.ring[((s.pos+i)%s.win)*s.channels+c]
		}
		frame.Quality[c] = s.analyzer.Analyze(s.scratch, frame.BandPower[c*eeg.NumBands:(c+1)*eeg.NumBands])
		if s.linkPairs != nil {
			copy(s.linkWindows[c*s.win:], s.scratch)
		}
	}
	if s.linkPairs != nil {
		frame.Links = computeLinks(s.linkWindows, s.win, frame.Quality, s.linkPairs, s.linkScratch)
	}
	return frame
}

// windowTime is the moment a window ends, on the recording's own clock: the
// session start plus the samples before it. It does not depend on replay speed,
// reconnects or which processor computed it, so a repeated event is exactly
// equal and Postgres keeps only one copy.
func (s *session) windowTime(windowIndex uint64) time.Time {
	return s.sampleTime(windowIndex * uint64(s.hop))
}

// sampleTime is the moment of one sample on the recording's own clock: the
// session start plus the samples before it.
// Used by: windowTime, markerEvents.
func (s *session) sampleTime(sampleIndex uint64) time.Time {
	seconds := float64(sampleIndex) / s.rate
	return s.startedAt.Add(time.Duration(seconds * float64(time.Second)))
}

// marker is one marker of a batch after merging: markers on the same sample
// are one marker with their labels joined by " + ".
// Built by: mergeMarkers. Used by: session.markerEvents, session.startEpochs.
type marker struct {
	sample   uint64
	label    string
	duration float64
}

// mergeMarkers returns the batch's markers, one per sample. Postgres keeps one
// marker event and one epoch per session and sample, so two markers on the
// same sample have to become one.
// Used by: session.add.
func mergeMarkers(b *neurov1.SampleBatch) []marker {
	var list []marker
next:
	for _, m := range b.Markers {
		for i := range list {
			if list[i].sample == m.SampleIndex {
				list[i].label += " + " + m.Label
				list[i].duration = max(list[i].duration, m.DurationSeconds)
				continue next
			}
		}
		list = append(list, marker{m.SampleIndex, m.Label, m.DurationSeconds})
	}
	return list
}

// markerEvents turns a batch's markers into "marker" events, timed at the
// marker's own sample. A batch read twice gives exactly the same events.
// Used by: add.
func (s *session) markerEvents(list []marker) []*neurov1.Event {
	var events []*neurov1.Event
	for _, m := range list {
		body, _ := json.Marshal(map[string]any{"label": m.label, "sample": m.sample, "duration": m.duration})
		events = append(events, &neurov1.Event{
			SessionId:     s.id,
			Type:          "marker",
			TimeUnixNanos: s.sampleTime(m.sample).UnixNano(),
			DetailsJson:   string(body),
		})
	}
	return events
}

// detect turns one frame into events: a sensor going bad or recovering, a bad
// sensor changing reason (noisy to flat, or back), alpha rising over the
// back of the head, and the start of an eye movement or of muscle activity.
// eye and eyeSwing come from eyeMovement for the same window.
func (s *session) detect(f *neurov1.FeatureFrame, eye bool, eyeSwing float64) []*neurov1.Event {
	var events []*neurov1.Event
	at := s.windowTime(f.WindowIndex).UnixNano()
	emit := func(kind, channel string, details map[string]any) {
		body, _ := json.Marshal(details)
		events = append(events, &neurov1.Event{
			SessionId:     s.id,
			Type:          kind,
			Channel:       channel,
			TimeUnixNanos: at,
			DetailsJson:   string(body),
		})
	}
	markBad := func(c int, q neurov1.Quality) {
		s.bad[c], s.badQ[c] = true, q
		reason := "flat"
		if q == neurov1.Quality_QUALITY_NOISY {
			reason = "noisy"
		}
		emit("channel_bad", s.names[c], map[string]any{"reason": reason, "window": f.WindowIndex})
	}

	for c, q := range f.Quality {
		if q == s.lastQ[c] {
			s.sameRun[c]++
		} else {
			s.lastQ[c], s.sameRun[c] = q, 1
		}

		if q == neurov1.Quality_QUALITY_OK {
			s.badRun[c] = 0
			if s.bad[c] && s.sameRun[c] >= recoveredAfterWindows {
				s.bad[c] = false
				emit("channel_recovered", s.names[c], map[string]any{"window": f.WindowIndex})
			}
			continue
		}

		s.badRun[c]++
		switch {
		case !s.bad[c] && s.badRun[c] >= badAfterWindows:
			markBad(c, q)
		case s.bad[c] && q != s.badQ[c] && s.sameRun[c] >= badAfterWindows:
			markBad(c, q) // still bad, for a new reason
		}
	}

	if len(s.back) > 0 {
		var alpha float64
		for _, c := range s.back {
			alpha += float64(f.BandPower[c*eeg.NumBands+eeg.AlphaIndex])
		}
		alpha /= float64(len(s.back))

		if s.alphaCount >= alphaWarmupWindows {
			baseline := s.alphaSum / float64(s.alphaCount)
			if alpha > alphaRiseRatio*baseline {
				s.alphaAbove++
				if s.alphaAbove == alphaRiseWindows { // fires once per rise
					emit("alpha_rise", "", map[string]any{
						"alpha":    math.Round(alpha*1000) / 1000,
						"baseline": math.Round(baseline*1000) / 1000,
						"window":   f.WindowIndex,
					})
				}
			} else {
				s.alphaAbove = 0
			}
		}
		s.alphaSum += alpha
		s.alphaCount++
	}

	// Artifacts fire once, when they start.
	if eye && !s.eyeActive {
		emit("eye_movement", "", map[string]any{"frontal_uv": math.Round(eyeSwing), "window": f.WindowIndex})
	}
	s.eyeActive = eye

	if tense := s.muscleSensors(f); len(tense) >= muscleMinSensors {
		s.muscleRun++
		if s.muscleRun == muscleAfterWindows {
			emit("muscle", "", map[string]any{
				"sensors": len(tense), "channels": tense[:min(len(tense), muscleListed)], "window": f.WindowIndex,
			})
		}
	} else {
		s.muscleRun = 0
	}
	return events
}
