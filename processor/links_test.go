package main

import (
	"math"
	"math/rand"
	"testing"

	neurov1 "github.com/neurohalo/neurohalo/gen/neuro/v1"
)

const testWin = 320

// allPairs lists every pair a < b of n channels.
func allPairs(n int) [][2]int {
	var out [][2]int
	for a := 0; a < n; a++ {
		for b := a + 1; b < n; b++ {
			out = append(out, [2]int{a, b})
		}
	}
	return out
}

// okQuality returns n OK flags.
func okQuality(n int) []neurov1.Quality {
	q := make([]neurov1.Quality, n)
	for i := range q {
		q[i] = neurov1.Quality_QUALITY_OK
	}
	return q
}

// noise fills channels x testWin values with independent random noise.
func noise(channels int, seed int64) []float32 {
	rng := rand.New(rand.NewSource(seed))
	w := make([]float32, channels*testWin)
	for i := range w {
		w[i] = float32(rng.NormFloat64() * 20)
	}
	return w
}

// TestLinksIdenticalChannels checks two identical channels give strength 1.
func TestLinksIdenticalChannels(t *testing.T) {
	w := noise(3, 1)
	copy(w[2*testWin:], w[0:testWin])
	links := computeLinks(w, testWin, okQuality(3), allPairs(3), make([]float64, len(w)))
	if len(links) == 0 || links[0].A != 0 || links[0].B != 2 {
		t.Fatalf("strongest link = %v, want 0-2", links)
	}
	if math.Abs(float64(links[0].Strength)-1) > 1e-6 {
		t.Fatalf("strength = %v, want 1", links[0].Strength)
	}
}

// TestLinksFlatChannelSkipped checks a flat channel is in no pair, both when
// its quality says FLAT and when only its zero variance gives it away.
func TestLinksFlatChannelSkipped(t *testing.T) {
	for _, q := range []neurov1.Quality{neurov1.Quality_QUALITY_FLAT, neurov1.Quality_QUALITY_OK} {
		w := noise(4, 2)
		for i := 0; i < testWin; i++ {
			w[1*testWin+i] = 7 // constant
		}
		quality := okQuality(4)
		quality[1] = q
		for _, l := range computeLinks(w, testWin, quality, allPairs(4), make([]float64, len(w))) {
			if l.A == 1 || l.B == 1 {
				t.Fatalf("quality %v: flat channel 1 in link %v", q, l)
			}
			s := float64(l.Strength)
			if math.IsNaN(s) || math.IsInf(s, 0) || s < 0 || s > 1 {
				t.Fatalf("bad strength %v", s)
			}
		}
	}
}

// TestLinksAtMost50 checks 64 channels (2016 pairs) give no more than 50
// links, strongest first.
func TestLinksAtMost50(t *testing.T) {
	w := noise(64, 3)
	links := computeLinks(w, testWin, okQuality(64), allPairs(64), make([]float64, len(w)))
	if len(links) != maxLinks {
		t.Fatalf("got %d links, want %d", len(links), maxLinks)
	}
	for i := 1; i < len(links); i++ {
		if links[i].Strength > links[i-1].Strength {
			t.Fatalf("links not sorted at %d", i)
		}
	}
}

// TestAllowedPairsSkipsNeighbors checks sensors closer than the minimum angle
// are never paired, and unknown sensors are left out.
func TestAllowedPairsSkipsNeighbors(t *testing.T) {
	lc := &linkConfig{minAngleDeg: 40, positions: map[string][3]float64{
		"A": {1, 0, 0},
		"B": {math.Cos(0.3), math.Sin(0.3), 0}, // about 17 degrees from A
		"C": {0, 1, 0},                         // 90 degrees from A
	}}
	pairs := lc.allowedPairs([]string{"A", "B", "C", "X"})
	want := [][2]int{{0, 2}, {1, 2}}
	if len(pairs) != len(want) || pairs[0] != want[0] || pairs[1] != want[1] {
		t.Fatalf("pairs = %v, want %v", pairs, want)
	}
}
