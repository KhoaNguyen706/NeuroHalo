package main

import (
	"math"
	"testing"
	"time"

	"github.com/neurohalo/neurohalo/internal/eeg"

	neurov1 "github.com/neurohalo/neurohalo/gen/neuro/v1"
)

// feedEpochs sends `seconds` of signal into a session in 100 ms batches, with
// a marker "cue" at each of the given sample positions, and returns the
// epochs that came out. value gives sensor c's microvolts at sample n.
func feedEpochs(s *session, seconds float64, markerSamples []uint64, value func(c int, n int) float64) []*neurov1.Epoch {
	var epochs []*neurov1.Epoch
	per := 16
	for seq := 0; seq < int(seconds*10); seq++ {
		b := &neurov1.SampleBatch{Seq: uint64(seq), FirstSampleIndex: uint64(seq * per), NumSamples: uint32(per)}
		for i := 0; i < per; i++ {
			for c := 0; c < s.channels; c++ {
				b.Values = append(b.Values, float32(value(c, seq*per+i)))
			}
		}
		for _, m := range markerSamples {
			if m >= b.FirstSampleIndex && m < b.FirstSampleIndex+uint64(per) {
				b.Markers = append(b.Markers, &neurov1.Marker{SampleIndex: m, Label: "cue"})
			}
		}
		epochs = append(epochs, s.add(b).epochs...)
	}
	return epochs
}

// A 10 Hz rhythm that halves in size exactly at the marker: the windows before
// the marker must show full alpha power, the windows wholly after it a quarter
// of that (power goes with the square of the size).
func TestEpochShowsAlphaDropAfterMarker(t *testing.T) {
	const marker = 803 // 5.02 s: deliberately not on a batch or window boundary
	s := newSession("t", testRate, []string{"C3", "C4"}, time.Unix(100, 0), nil)
	epochs := feedEpochs(s, 12, []uint64{marker}, func(c, n int) float64 {
		size := 20.0
		if n > marker {
			size = 10
		}
		return size * math.Sin(2*math.Pi*10*float64(n)/testRate)
	})
	if len(epochs) != 1 {
		t.Fatalf("got %d epochs, want 1", len(epochs))
	}
	e := epochs[0]
	if e.Label != "cue" || e.MarkerSample != marker || e.Steps != epochSteps || e.PreSteps != epochPreSteps {
		t.Fatalf("epoch header: label %q sample %d steps %d pre %d", e.Label, e.MarkerSample, e.Steps, e.PreSteps)
	}
	if want := time.Unix(100, 0).Add(time.Duration(float64(marker) / testRate * float64(time.Second))).UnixNano(); e.TimeUnixNanos != want {
		t.Errorf("time %d, want %d", e.TimeUnixNanos, want)
	}
	if e.StepSeconds != 0.25 || e.WindowSeconds != 1 {
		t.Errorf("step %g s, window %g s, want 0.25 and 1", e.StepSeconds, e.WindowSeconds)
	}
	alpha := func(step, c int) float64 {
		return float64(e.BandPower[(step*s.channels+c)*eeg.NumBands+eeg.AlphaIndex])
	}
	before := alpha(0, 0)
	if before <= 0 {
		t.Fatal("no alpha power before the marker")
	}
	for step := 0; step < epochSteps; step++ {
		for c := 0; c < 2; c++ {
			ratio := alpha(step, c) / before
			switch {
			case step < epochPreSteps && math.Abs(ratio-1) > 0.01:
				t.Errorf("step %d sensor %d: before the marker, alpha is %.3f of the first window, want 1", step, c, ratio)
			// Windows ending 1 s or more after the marker hold only the smaller rhythm.
			case step >= epochPreSteps+3 && math.Abs(ratio-0.25) > 0.01:
				t.Errorf("step %d sensor %d: after the marker, alpha is %.3f of the first window, want 0.25", step, c, ratio)
			}
		}
	}
	if e.Bad[0] || e.Bad[1] {
		t.Errorf("bad = %v, want none", e.Bad)
	}
}

func TestEpochNeedsTwoSecondsBeforeMarker(t *testing.T) {
	s := newSession("t", testRate, []string{"Cz"}, time.Unix(0, 0), nil)
	// The first marker comes 1 s in, before the buffer is full: no epoch. The second is fine.
	epochs := feedEpochs(s, 12, []uint64{160, 800}, func(c, n int) float64 {
		return 20 * math.Sin(2*math.Pi*10*float64(n)/testRate)
	})
	if len(epochs) != 1 || epochs[0].MarkerSample != 800 {
		t.Fatalf("got %d epochs, want only the one at sample 800", len(epochs))
	}
}

func TestEpochMarksFlatSensorBad(t *testing.T) {
	s := newSession("t", testRate, []string{"C3", "C4"}, time.Unix(0, 0), nil)
	// C4 goes flat 2 s after the marker.
	epochs := feedEpochs(s, 12, []uint64{800}, func(c, n int) float64 {
		if c == 1 && n > 800+320 {
			return 0
		}
		return 20 * math.Sin(2*math.Pi*10*float64(n)/testRate)
	})
	if len(epochs) != 1 {
		t.Fatalf("got %d epochs, want 1", len(epochs))
	}
	if epochs[0].Bad[0] || !epochs[0].Bad[1] {
		t.Errorf("bad = %v, want only the second sensor", epochs[0].Bad)
	}
}

func TestOverlappingEpochsBothFinish(t *testing.T) {
	s := newSession("t", testRate, []string{"Cz"}, time.Unix(0, 0), nil)
	// Markers 1 s apart: their 5-second epochs overlap.
	epochs := feedEpochs(s, 12, []uint64{800, 960}, func(c, n int) float64 {
		return 20 * math.Sin(2*math.Pi*10*float64(n)/testRate)
	})
	if len(epochs) != 2 || epochs[0].MarkerSample != 800 || epochs[1].MarkerSample != 960 {
		t.Fatalf("got %d epochs, want the ones at samples 800 and 960 in that order", len(epochs))
	}
}

// Each sample's value is its own position (plus 10,000 on the second sensor),
// so the raw stretch must hold exactly the samples from 0.5 s before the
// marker to 1.3 s after it, sensor by sensor, in order.
func TestEpochKeepsRawStretch(t *testing.T) {
	const marker = 803
	s := newSession("t", testRate, []string{"C3", "C4"}, time.Unix(0, 0), nil)
	epochs := feedEpochs(s, 12, []uint64{marker}, func(c, n int) float64 {
		return float64(n + 10000*c)
	})
	if len(epochs) != 1 {
		t.Fatalf("got %d epochs, want 1", len(epochs))
	}
	e := epochs[0]
	if e.VoltagePre != 80 || e.VoltageSamples != 288 || len(e.Voltage) != 288*2 {
		t.Fatalf("pre %d, samples %d, values %d; want 80, 288, 576", e.VoltagePre, e.VoltageSamples, len(e.Voltage))
	}
	for i := 0; i < 288; i++ {
		for c := 0; c < 2; c++ {
			if got, want := e.Voltage[i*2+c], float32(marker-80+i+10000*c); got != want {
				t.Fatalf("sample %d sensor %d = %g, want %g", i, c, got, want)
			}
		}
	}
}
