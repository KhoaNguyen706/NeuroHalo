package main

import (
	"math"
	"testing"
)

// The low-pass must be MNE's default 30 Hz filter at 160 Hz, tap for tap.
func TestLowpassKernelMatchesMNE(t *testing.T) {
	h := lowpassKernel(160, 30)
	if len(h) != 71 {
		t.Fatalf("%d taps, want 71", len(h))
	}
	var sum float64
	for i, v := range h {
		sum += v
		if math.Abs(v-h[len(h)-1-i]) > 1e-15 {
			t.Fatalf("tap %d is not mirrored", i)
		}
	}
	if math.Abs(sum-1) > 1e-12 {
		t.Errorf("gain at 0 Hz = %g, want 1", sum)
	}
	want := map[int]float64{35: wantMiddleTap, 0: wantFirstTaps[0], 1: wantFirstTaps[1], 2: wantFirstTaps[2]}
	for i, v := range want {
		if math.Abs(h[i]-v) > 1e-12 {
			t.Errorf("tap %d = %.15g, want %.15g", i, h[i], v)
		}
	}
}

// erpRows builds a stretch of n rows x 1 channel from a function of the row.
func erpRows(label string, n int, f func(row int) float64) voltageRow {
	v := make([]float32, n)
	for i := range v {
		v[i] = float32(f(i))
	}
	return voltageRow{label: label, voltage: v}
}

func TestAverageERP(t *testing.T) {
	// A 1-tap kernel keeps the arithmetic visible: rows 2..6, marker at row 4,
	// baseline rows 2..4.
	sh := erpShape{channels: 1, pre: 4, from: 2, to: 6, baseline: 3, kernel: []float64{1}, binFirst: []int{0, 2}, binLast: []int{2, 4}}
	step := func(row int) float64 { // 10 µV before the marker, 40 µV from the row after it
		if row > 4 {
			return 40
		}
		return 10
	}
	rows := []voltageRow{
		erpRows("cue", 8, step),
		erpRows("cue", 8, func(row int) float64 { return step(row) + 5 }),   // the same, shifted: the baseline removes it
		erpRows("cue", 8, func(row int) float64 { return 300 * step(row) }), // 9,000 µV jump: rejected at 100
		erpRows("flat", 8, func(int) float64 { return 7 }),                  // flat: always left out
	}
	got := averageERP(rows, sh, 100)
	if len(got) != 2 || got[0].Label != "cue" || got[1].Label != "flat" {
		t.Fatalf("labels %+v", got)
	}
	cue := got[0]
	if cue.Trials != 3 || cue.Good[0] != 2 {
		t.Errorf("cue: %d trials, %d good; want 3 and 2", cue.Trials, cue.Good[0])
	}
	want := []float32{0, 0, 0, 30, 30}
	for i, v := range want {
		if cue.Mean[i] != v {
			t.Errorf("cue mean[%d] = %g, want %g", i, cue.Mean[i], v)
		}
	}
	// Both kept trials are the same once baselined: no spread, SME 0.
	if cue.SME[0] != 0 || cue.SME[1] != 0 {
		t.Errorf("cue SME %v, want [0 0]", cue.SME)
	}
	if got[1].Good[0] != 0 || got[1].SME[0] != -1 {
		t.Errorf("flat trial counted, or given an SME")
	}
	// With rejection off, the large trial is kept.
	if all := averageERP(rows, sh, 0); all[0].Good[0] != 3 {
		t.Errorf("reject 0: %d good, want 3", all[0].Good[0])
	}
}

// Two trials whose window means after the marker are 10 and 20 µV: their
// sample standard deviation is 7.07 µV, so the SME is 7.07 / sqrt(2) = 5 µV.
// Row 2 is on the boundary, so as in ERPLAB it also counts in the first
// window: means 10/3 and 20/3, an SME of 5/3 µV.
func TestERPSME(t *testing.T) {
	sh := erpShape{channels: 1, pre: 1, from: 0, to: 3, baseline: 2, kernel: []float64{1}, binFirst: []int{0, 2}, binLast: []int{2, 3}}
	rows := []voltageRow{
		erpRows("cue", 4, func(row int) float64 { return []float64{0, 0, 10, 10}[row] }),
		erpRows("cue", 4, func(row int) float64 { return []float64{0, 0, 20, 20}[row] }),
	}
	got := averageERP(rows, sh, 0)[0]
	if math.Abs(float64(got.SME[1])-5) > 1e-6 || math.Abs(float64(got.SME[0])-5.0/3) > 1e-6 {
		t.Errorf("SME %v, want [1.667 5]", got.SME)
	}
}

func TestSMEWindows(t *testing.T) {
	// 160 Hz from -0.2 to 0.8 s: ERPLAB's 10 windows, -200 to -100 ms up to
	// 700 to 800 ms, each 17 samples, sharing the sample on each boundary.
	var times []float64
	for i := -32; i <= 128; i++ {
		times = append(times, float64(i)/160)
	}
	starts, first, last := smeWindows(times)
	if len(starts) != 10 || math.Abs(starts[0]+0.2) > 1e-9 || math.Abs(starts[9]-0.7) > 1e-9 {
		t.Fatalf("starts %v", starts)
	}
	if first[0] != 0 || last[0] != 16 || first[1] != 16 || first[9] != 144 || last[9] != 160 {
		t.Errorf("first %v last %v", first, last)
	}

	// 256 Hz from -51 to 204 samples: -0.199 s is after -200 ms, so ERPLAB
	// starts at -100 ms; the 700 ms window would end after 0.797 s and is dropped.
	times = times[:0]
	for i := -51; i <= 204; i++ {
		times = append(times, float64(i)/256)
	}
	starts, first, last = smeWindows(times)
	if len(starts) != 8 || math.Abs(starts[0]+0.1) > 1e-9 || math.Abs(starts[7]-0.6) > 1e-9 {
		t.Fatalf("256 Hz starts %v", starts)
	}
	if times[first[0]] > -0.098 || times[first[0]] < -0.102 || times[last[7]] < 0.698 || times[last[7]] > 0.702 {
		t.Errorf("256 Hz: first window from %.4f, last to %.4f", times[first[0]], times[last[7]])
	}
}

// From mne.filter.create_filter(None, 160, None, 30) (MNE 1.13, scipy 1.18),
// which equals scipy.signal.firwin(71, 33.75, window="hamming", fs=160).
const wantMiddleTap = 0.42139444437943957

var wantFirstTaps = [3]float64{0.00048804653071331234, 0.0006750476126148205, -0.00020459059479910598}
