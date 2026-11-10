package eeg

import (
	"math"
	"testing"

	neurov1 "github.com/neurohalo/neurohalo/gen/neuro/v1"
)

// sine returns n samples of a sine wave at the given frequency and amplitude.
func sine(hz, amplitude float64, n int, rate float64) []float32 {
	out := make([]float32, n)
	for i := range out {
		out[i] = float32(amplitude * math.Sin(2*math.Pi*hz*float64(i)/rate))
	}
	return out
}

// TestAnalyze checks that a pure tone lands in the right band and that flat
// and noisy signals get the right quality.
func TestAnalyze(t *testing.T) {
	const n, rate = 320, 160.0
	a := NewAnalyzer(n, rate)
	out := make([]float32, NumBands)

	cases := []struct {
		name     string
		window   []float32
		wantBand int // -1: all zeros
		wantQ    neurov1.Quality
	}{
		{"10 Hz is alpha", sine(10, 20, n, rate), AlphaIndex, neurov1.Quality_QUALITY_OK},
		{"2 Hz is delta", sine(2, 20, n, rate), 0, neurov1.Quality_QUALITY_OK},
		{"20 Hz is beta", sine(20, 20, n, rate), 3, neurov1.Quality_QUALITY_OK},
		{"normal swings are ok", sine(10, 150, n, rate), AlphaIndex, neurov1.Quality_QUALITY_OK},
		{"big swings are noisy", sine(10, 300, n, rate), AlphaIndex, neurov1.Quality_QUALITY_NOISY},
		{"zeros are flat", make([]float32, n), -1, neurov1.Quality_QUALITY_FLAT},
	}
	for _, c := range cases {
		q := a.Analyze(c.window, out)
		if q != c.wantQ {
			t.Errorf("%s: quality %v, want %v", c.name, q, c.wantQ)
		}
		for i, p := range out {
			switch {
			case c.wantBand == -1 && p != 0:
				t.Errorf("%s: band %d = %v, want 0", c.name, i, p)
			case i == c.wantBand && p < 0.99:
				t.Errorf("%s: band %d = %v, want about 1", c.name, i, p)
			}
		}
	}
}

// TestBackOfHead checks which sensor names count as back of the head.
func TestBackOfHead(t *testing.T) {
	for name, want := range map[string]bool{"O1": true, "PO7": true, "Pz": true, "Fp1": false, "Cz": false, "CP3": false} {
		if BackOfHead(name) != want {
			t.Errorf("BackOfHead(%q) = %v, want %v", name, !want, want)
		}
	}
}
