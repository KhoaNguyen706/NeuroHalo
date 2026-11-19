package main

import (
	"testing"

	"github.com/neurohalo/neurohalo/internal/eeg"
)

// epochOf builds one epoch row of 2 steps x 2 channels whose alpha power is
// the four given values (step 0: channel 0, channel 1; step 1: channel 0,
// channel 1). Every other band is 99, to catch reading the wrong band.
func epochOf(label string, bad []bool, alpha ...float32) epochRow {
	power := make([]float32, 2*2*eeg.NumBands)
	for i := range power {
		power[i] = 99
	}
	for i, v := range alpha {
		power[i*eeg.NumBands+eeg.AlphaIndex] = v
	}
	return epochRow{label: label, bandPower: power, bad: bad}
}

func TestAverageResponses(t *testing.T) {
	rows := []epochRow{
		epochOf("T2", []bool{false, false}, 1, 1, 1, 1),
		epochOf("T1", []bool{false, false}, 10, 20, 30, 40),
		epochOf("T1", []bool{false, true}, 20, 999, 50, 999), // channel 1 is bad in this trial
	}
	got := averageResponses(rows, 2, 2, eeg.AlphaIndex)
	if len(got) != 2 || got[0].Label != "T1" || got[1].Label != "T2" {
		t.Fatalf("labels %+v, want T1 then T2", got)
	}
	t1 := got[0]
	if t1.Trials != 2 || t1.Good[0] != 2 || t1.Good[1] != 1 {
		t.Errorf("T1: trials %d good %v, want 2 trials, good [2 1]", t1.Trials, t1.Good)
	}
	// Channel 0 averages both trials; channel 1 only the first.
	want := []float32{15, 20, 40, 40}
	for i, v := range want {
		if t1.Power[i] != v {
			t.Errorf("T1 power[%d] = %g, want %g", i, t1.Power[i], v)
		}
	}
}
