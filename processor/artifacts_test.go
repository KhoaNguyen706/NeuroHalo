package main

import (
	"math"
	"testing"
	"time"

	neurov1 "github.com/neurohalo/neurohalo/gen/neuro/v1"
)

const testRate = 160.0

// feed sends `seconds` of signal into a session in 100 ms batches and returns
// the types of the events that came out. value gives sensor c's microvolts at second t.
func feed(s *session, seconds float64, value func(c int, t float64) float64) []string {
	var kinds []string
	per := 16
	for seq := 0; seq < int(seconds*10); seq++ {
		b := &neurov1.SampleBatch{Seq: uint64(seq), FirstSampleIndex: uint64(seq * per), NumSamples: uint32(per)}
		for i := 0; i < per; i++ {
			t := float64(seq*per+i) / testRate
			for c := 0; c < s.channels; c++ {
				b.Values = append(b.Values, float32(value(c, t)))
			}
		}
		for _, e := range s.add(b).events {
			kinds = append(kinds, e.Type)
		}
	}
	return kinds
}

// count returns how many times kind appears in kinds.
func count(kinds []string, kind string) int {
	n := 0
	for _, k := range kinds {
		if k == kind {
			n++
		}
	}
	return n
}

// alpha is a quiet 10 Hz rhythm, the same on every sensor.
func alpha(t float64) float64 { return 20 * math.Sin(2*math.Pi*10*t) }

func TestEyeMovementFiresOncePerBlink(t *testing.T) {
	names := []string{"Fp1", "Fp2", "Fpz", "Cz", "O1", "O2"}
	s := newSession("t", testRate, names, time.Unix(0, 0), nil)
	// A 300 uV bump lasting 0.3 s at the frontal sensors, at seconds 4 and 7.
	blink := func(t float64) float64 {
		for _, at := range []float64{4, 7} {
			if d := t - at; d >= 0 && d < 0.3 {
				return 300 * math.Sin(math.Pi*d/0.3)
			}
		}
		return 0
	}
	kinds := feed(s, 10, func(c int, t float64) float64 {
		if c < 3 {
			return alpha(t) + blink(t)
		}
		return alpha(t)
	})
	if got := count(kinds, "eye_movement"); got != 2 {
		t.Fatalf("eye_movement events = %d, want 2 (all events: %v)", got, kinds)
	}
}

func TestSwingOnWholeHeadIsNotAnEyeMovement(t *testing.T) {
	names := []string{"Fp1", "Fp2", "Fpz", "Cz", "O1", "O2"}
	s := newSession("t", testRate, names, time.Unix(0, 0), nil)
	// The same bump on every sensor is movement or drift, not the eyes.
	kinds := feed(s, 8, func(c int, t float64) float64 {
		if d := t - 4; d >= 0 && d < 0.3 {
			return alpha(t) + 300*math.Sin(math.Pi*d/0.3)
		}
		return alpha(t)
	})
	if got := count(kinds, "eye_movement"); got != 0 {
		t.Fatalf("eye_movement events = %d, want 0", got)
	}
}

func TestMuscleFiresOnceWhenSeveralSensorsTurnFast(t *testing.T) {
	names := []string{"T7", "T8", "FT7", "Cz", "O1", "O2"}
	s := newSession("t", testRate, names, time.Unix(0, 0), nil)
	// From second 4 to 8, three sensors carry strong 38 Hz activity.
	kinds := feed(s, 12, func(c int, t float64) float64 {
		if c < 3 && t >= 4 && t < 8 {
			return alpha(t) + 60*math.Sin(2*math.Pi*38*t)
		}
		return alpha(t)
	})
	if got := count(kinds, "muscle"); got != 1 {
		t.Fatalf("muscle events = %d, want 1 (all events: %v)", got, kinds)
	}
}

func TestQuietSignalHasNoArtifacts(t *testing.T) {
	names := []string{"Fp1", "Fp2", "Fpz", "T7", "T8", "O1"}
	s := newSession("t", testRate, names, time.Unix(0, 0), nil)
	kinds := feed(s, 10, func(c int, t float64) float64 { return alpha(t) })
	if count(kinds, "eye_movement")+count(kinds, "muscle") != 0 {
		t.Fatalf("events on a quiet signal: %v", kinds)
	}
}
