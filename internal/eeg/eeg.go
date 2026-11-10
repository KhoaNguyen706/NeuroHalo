// Package eeg holds the signal math: rhythm strength per band and signal quality.
package eeg

import (
	"math"
	"strings"

	"gonum.org/v1/gonum/dsp/fourier"

	neurov1 "github.com/neurohalo/neurohalo/gen/neuro/v1"
)

// Band is one rhythm band and its frequency range.
// Used by: the Bands table, which NewAnalyzer reads to assign FFT bins.
type Band struct {
	Name   string
	Lo, Hi float64 // Hz, Lo inclusive, Hi exclusive (the last band includes its Hi)
}

// Common ranges. Labs vary them slightly.
var Bands = []Band{
	{"delta", 1, 4},
	{"theta", 4, 8},
	{"alpha", 8, 13},
	{"beta", 13, 30},
	{"gamma", 30, 45},
}

const (
	NumBands   = 5
	AlphaIndex = 2

	WindowSeconds = 2.0 // signal kept per sensor
	HopSeconds    = 0.5 // time between recomputes

	// Flat is still the design's starting guess. Noisy was 200 uV, but in the
	// sample recordings the median 2-second peak-to-peak is 216 to 248 uV, so
	// 200 flagged most healthy windows. At 500 uV it flags 0.3% of eyes-closed
	// windows and 3.5% of eyes-open ones.
	FlatStdMicrovolts        = 0.5
	NoisyPeakToPeakMicrovolt = 500.0
)

// WindowSamples is how many samples one analysis window holds at this rate.
// Used by: the processor's newSession.
func WindowSamples(sampleRateHz float64) int {
	return int(math.Round(WindowSeconds * sampleRateHz))
}

// HopSamples is how many samples apart two windows are at this rate. Window
// number i ends at sample i x HopSamples.
// Used by: the processor's newSession and the api's store, which turns a
// window number back into a time.
func HopSamples(sampleRateHz float64) int {
	return int(math.Round(HopSeconds * sampleRateHz))
}

// BackOfHead reports whether a sensor sits over the back of the head:
// names starting with O, PO or P ("Fp1" does not count).
func BackOfHead(channel string) bool {
	return strings.HasPrefix(channel, "O") || strings.HasPrefix(channel, "P")
}

// Analyzer computes band power for windows of one fixed length and sample rate.
// It is not safe for use from several goroutines.
// Created by: NewAnalyzer (called from the processor's newSession).
// Used by: Analyze (its method), called from the processor's session.compute.
type Analyzer struct {
	fft     *fourier.FFT
	hann    []float64
	in      []float64
	coef    []complex128
	binBand []int // band index for each FFT bin, -1 when outside 1 to 45 Hz
}

// NewAnalyzer prepares everything that depends only on the window length and
// sample rate: the FFT plan, the Hann window, and which band each FFT bin
// belongs to.
func NewAnalyzer(windowSamples int, sampleRateHz float64) *Analyzer {
	a := &Analyzer{
		fft:     fourier.NewFFT(windowSamples),
		hann:    make([]float64, windowSamples),
		in:      make([]float64, windowSamples),
		coef:    make([]complex128, windowSamples/2+1),
		binBand: make([]int, windowSamples/2+1),
	}
	for i := range a.hann {
		a.hann[i] = 0.5 * (1 - math.Cos(2*math.Pi*float64(i)/float64(windowSamples)))
	}
	last := len(Bands) - 1
	for k := range a.binBand {
		f := float64(k) * sampleRateHz / float64(windowSamples)
		a.binBand[k] = -1
		for i, b := range Bands {
			if f >= b.Lo && (f < b.Hi || (i == last && f == b.Hi)) {
				a.binBand[k] = i
				break
			}
		}
	}
	return a
}

// Analyze takes one sensor's window in microvolts. It writes the relative
// strength of each band (0 to 1, summing to 1) into out and returns the quality.
// A sensor with no power between 1 and 45 Hz gets all zeros.
func (a *Analyzer) Analyze(window []float32, out []float32) neurov1.Quality {
	lo, hi := float64(window[0]), float64(window[0])
	var sum float64
	for _, v := range window {
		x := float64(v)
		sum += x
		lo, hi = math.Min(lo, x), math.Max(hi, x)
	}
	mean := sum / float64(len(window))
	var variance float64
	for i, v := range window {
		d := float64(v) - mean
		variance += d * d
		a.in[i] = d * a.hann[i] // remove the offset, then apply the Hann window
	}
	std := math.Sqrt(variance / float64(len(window)))

	a.fft.Coefficients(a.coef, a.in)
	var power [NumBands]float64
	var total float64
	for k, c := range a.coef {
		if b := a.binBand[k]; b >= 0 {
			p := real(c)*real(c) + imag(c)*imag(c)
			power[b] += p
			total += p
		}
	}
	for i := range power {
		if total > 0 {
			out[i] = float32(power[i] / total)
		} else {
			out[i] = 0
		}
	}

	switch {
	case std < FlatStdMicrovolts:
		return neurov1.Quality_QUALITY_FLAT
	case hi-lo > NoisyPeakToPeakMicrovolt:
		return neurov1.Quality_QUALITY_NOISY
	}
	return neurov1.Quality_QUALITY_OK
}
