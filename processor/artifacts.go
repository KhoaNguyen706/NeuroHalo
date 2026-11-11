package main

import (
	"math"

	"github.com/neurohalo/neurohalo/internal/eeg"

	neurov1 "github.com/neurohalo/neurohalo/gen/neuro/v1"
)

// Artifacts are signals that come from the body, not the brain. Two kinds
// are flagged, each as one event when it starts:
//
//	eye_movement  a blink or eye movement: a large slow swing at the three
//	              sensors above the eyes that the back of the head does not share
//	muscle        jaw or scalp muscle tension: fast activity (30 to 45 Hz)
//	              taking a large share of the signal on several sensors at once
//
// The thresholds were set on the sample recordings (eyes open: 8 eye events a
// minute; eyes closed: 3 to 6, since eyes also move under closed lids). They
// are rules of thumb, not checked against hand-labelled data.
const (
	eyeSmoothSeconds   = 0.1   // moving average; at 50 and 60 Hz mains it also removes the hum
	eyeMinMicrovolts   = 100.0 // peak-to-peak of the smoothed frontal signal over the newest 0.5 s
	eyeFrontToBack     = 3.0   // and at least this many times the back of the head's
	muscleGammaShare   = 0.3   // a sensor counts when gamma is above this share of its 1 to 45 Hz power
	muscleMinSensors   = 3
	muscleAfterWindows = 2 // that many sensors for this many windows in a row -> muscle
	muscleListed       = 8 // sensors named in the event's details
	gammaIndex         = 4 // index of "gamma" in eeg.Bands
)

// frontPoleSensors returns the indexes of the sensors right above the eyes.
// Used by: newSession.
func frontPoleSensors(names []string) []int {
	var out []int
	for i, name := range names {
		if name == "Fp1" || name == "Fp2" || name == "Fpz" {
			out = append(out, i)
		}
	}
	return out
}

// groupSwing returns the peak-to-peak, in microvolts, of the average of the
// given sensors over the newest hop of samples, after a moving average of
// eyeSmoothSeconds. It reads the ring buffer, which must be full.
// Used by: eyeMovement.
func (s *session) groupSwing(sensors []int) float64 {
	k := max(1, int(math.Round(eyeSmoothSeconds*s.rate)))
	n := s.hop + k - 1 // samples needed for one hop of smoothed values
	if n > s.win {
		n = s.win
	}
	lo, hi := math.Inf(1), math.Inf(-1)
	var sum float64
	for i := 0; i < n; i++ {
		row := ((s.pos + s.win - n + i) % s.win) * s.channels
		var v float64
		for _, c := range sensors {
			v += float64(s.ring[row+c])
		}
		v /= float64(len(sensors))
		s.eyeScratch[i] = v
		sum += v
		if i >= k {
			sum -= s.eyeScratch[i-k]
		}
		if i >= k-1 {
			avg := sum / float64(k)
			lo, hi = math.Min(lo, avg), math.Max(hi, avg)
		}
	}
	return hi - lo
}

// eyeMovement reports whether the newest 0.5 s holds a blink or eye movement,
// and the size of the frontal swing in microvolts. It is false for a session
// without frontal-pole or back-of-head sensors.
// Used by: session.add, which passes the answer to detect.
func (s *session) eyeMovement() (bool, float64) {
	if len(s.frontPole) == 0 || len(s.back) == 0 {
		return false, 0
	}
	front := s.groupSwing(s.frontPole)
	return front > eyeMinMicrovolts && front > eyeFrontToBack*s.groupSwing(s.back), front
}

// muscleSensors lists the sensors whose signal in this frame is mostly fast
// activity. Bad sensors are left out: their spectrum means nothing.
// Used by: session.detect.
func (s *session) muscleSensors(f *neurov1.FeatureFrame) []string {
	var out []string
	for c, q := range f.Quality {
		if q == neurov1.Quality_QUALITY_OK && f.BandPower[c*eeg.NumBands+gammaIndex] > muscleGammaShare {
			out = append(out, s.names[c])
		}
	}
	return out
}
