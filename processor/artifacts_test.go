package main

import (
	"math"
	"testing"
	"time"

	neurov1 "github.com/neurohalo/neurohalo/gen/neuro/v1"
)

const testRate = 160.0

// feed sends `seconds` of signal into a session in 100 ms batches and returns
// the types of the events that came out. value gives sensor c's microvolts at
// second t. onFrame, when given, sees every frame.
func feed(s *session, seconds float64, value func(c int, t float64) float64, onFrame ...func(*neurov1.FeatureFrame)) []string {
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
		res := s.add(b)
		for _, e := range res.events {
			kinds = append(kinds, e.Type)
		}
		for _, f := range res.frames {
			for _, fn := range onFrame {
				fn(f)
			}
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

func TestEyeSwingsDoNotMarkFrontSensorsBad(t *testing.T) {
	names := []string{"Fp1", "Fp2", "Fpz", "AF3", "F7", "Cz", "O1", "O2"}
	s := newSession("t", testRate, names, time.Unix(0, 0), nil)
	// A 900 uV, 1-second eye movement every 3 s, larger than the 500 uV noisy
	// rule, at the four forehead sensors, and 60% of it at F7. The back of
	// the head swings 150 uV with its own slow drift.
	eyes := func(t float64) float64 {
		if d := math.Mod(t, 3) - 1; d >= 0 && d < 1 {
			return -900 * math.Sin(math.Pi*d)
		}
		return 0
	}
	var notOK []string
	kinds := feed(s, 30, func(c int, t float64) float64 {
		switch {
		case c < 4:
			return alpha(t) + eyes(t)
		case c == 4:
			return alpha(t) + 0.6*eyes(t)
		}
		return alpha(t) + 75*math.Sin(2*math.Pi*0.3*t)
	}, func(f *neurov1.FeatureFrame) {
		// The frames say OK too, so the page shows no red ring during a blink.
		for c, q := range f.Quality {
			if q != neurov1.Quality_QUALITY_OK {
				notOK = append(notOK, names[c])
			}
		}
	})
	if got := count(kinds, "channel_bad"); got != 0 {
		t.Fatalf("channel_bad events = %d, want 0 (all events: %v)", got, kinds)
	}
	if len(notOK) > 0 {
		t.Fatalf("frames with a sensor not OK during eye movements: %v", notOK)
	}
}

func TestNoisyForeheadSensorIsStillMarkedBad(t *testing.T) {
	names := []string{"Fp1", "Fp2", "Fpz", "Cz", "O1", "O2"}
	s := newSession("t", testRate, names, time.Unix(0, 0), nil)
	// Fp1 carries an 800 uV square wave from second 3: a loose sensor. Fp2
	// and Fpz do not share it, so it does not follow the eyes and is marked bad.
	kinds := feed(s, 15, func(c int, t float64) float64 {
		if c == 0 && t >= 3 {
			return alpha(t) + 400*math.Copysign(1, math.Sin(2*math.Pi*3*t))
		}
		return alpha(t)
	})
	if got := count(kinds, "channel_bad"); got != 1 {
		t.Fatalf("channel_bad events = %d, want 1 (all events: %v)", got, kinds)
	}
}

func TestNoisyBackSensorIsMarkedBadDuringBlinks(t *testing.T) {
	names := []string{"Fp1", "Fp2", "Fpz", "Cz", "O1", "O2"}
	s := newSession("t", testRate, names, time.Unix(0, 0), nil)
	// Blinks at the forehead do not slow down flagging a fault elsewhere: Cz
	// is marked bad within the usual 3 windows.
	var badAt float64 = -1
	per := 16
	for seq := 0; seq < 100; seq++ {
		b := &neurov1.SampleBatch{Seq: uint64(seq), FirstSampleIndex: uint64(seq * per), NumSamples: uint32(per)}
		for i := 0; i < per; i++ {
			tt := float64(seq*per+i) / testRate
			for c := 0; c < s.channels; c++ {
				v := alpha(tt)
				if c < 3 && math.Mod(tt, 1) < 0.3 {
					v += 300 * math.Sin(math.Pi*math.Mod(tt, 1)/0.3)
				}
				if c == 3 && tt >= 4 {
					v += 400 * math.Copysign(1, math.Sin(2*math.Pi*3*tt))
				}
				b.Values = append(b.Values, float32(v))
			}
		}
		for _, e := range s.add(b).events {
			if e.Type == "channel_bad" && badAt < 0 {
				if e.Channel != "Cz" {
					t.Fatalf("%s marked bad, want only Cz", e.Channel)
				}
				badAt = float64(seq+1) * 0.1
			}
		}
	}
	if badAt < 0 || badAt > 6 {
		t.Fatalf("Cz marked bad at %.1f s, want within 2 s of the fault at 4 s", badAt)
	}
}

func TestEyeExcuseRunsOutAfterTenSeconds(t *testing.T) {
	names := []string{"Fp1", "Fp2", "Fpz", "Cz", "O1", "O2"}
	s := newSession("t", testRate, names, time.Unix(0, 0), nil)
	// The three forehead sensors swing 900 uV together without a break from
	// second 2: no eye moves like that for long, so after 10 s of being
	// excused they are marked bad.
	kinds := feed(s, 20, func(c int, t float64) float64 {
		if c < 3 && t >= 2 {
			return alpha(t) + 450*math.Sin(2*math.Pi*0.7*t)
		}
		return alpha(t)
	})
	if got := count(kinds, "channel_bad"); got != 3 {
		t.Fatalf("channel_bad events = %d, want 3 (all events: %v)", got, kinds)
	}
}
