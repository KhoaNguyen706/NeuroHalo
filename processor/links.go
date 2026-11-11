package main

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"sort"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	neurov1 "github.com/neurohalo/neurohalo/gen/neuro/v1"
)

const maxLinks = 50 // strongest pairs kept per window

// linksComputeSeconds times computeLinks once per window.
// Observed by: session.compute.
var linksComputeSeconds = promauto.NewHistogram(prometheus.HistogramOpts{
	Name:    "links_compute_seconds",
	Help:    "Time to compute the correlation links of one window.",
	Buckets: prometheus.ExponentialBuckets(0.0001, 2, 12), // 0.1 ms to about 0.2 s
})

// linkConfig is what link computing needs from the environment: the sensor
// positions (the frontend's positions.json) and the minimum angle between
// two sensors for their pair to count.
// Built by: loadLinkConfig. Used by: newSession (through allowedPairs).
type linkConfig struct {
	positions   map[string][3]float64
	minAngleDeg float64
}

// loadLinkConfig reads LINKS_ENABLED, POSITIONS_FILE and MIN_LINK_ANGLE_DEG.
// It returns nil when links are off.
func loadLinkConfig() (*linkConfig, error) {
	on, _ := strconv.ParseBool(os.Getenv("LINKS_ENABLED"))
	if !on {
		return nil, nil
	}
	minAngle, err := strconv.ParseFloat(env("MIN_LINK_ANGLE_DEG", "40"), 64)
	if err != nil {
		return nil, fmt.Errorf("MIN_LINK_ANGLE_DEG: %w", err)
	}
	path := env("POSITIONS_FILE", "web/src/positions.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("positions: %w", err)
	}
	var positions map[string][3]float64
	if err := json.Unmarshal(data, &positions); err != nil {
		return nil, fmt.Errorf("positions %s: %w", path, err)
	}
	return &linkConfig{positions: positions, minAngleDeg: minAngle}, nil
}

// allowedPairs lists every channel pair (a < b) whose sensors are at least
// minAngleDeg apart on the head. Neighbors always correlate, so they are left
// out. A channel with no known position is left out of every pair.
// Called once per session by newSession.
func (lc *linkConfig) allowedPairs(names []string) [][2]int {
	minCos := math.Cos(lc.minAngleDeg * math.Pi / 180)
	var pairs [][2]int
	for a := range names {
		pa, okA := lc.positions[names[a]]
		if !okA {
			continue
		}
		for b := a + 1; b < len(names); b++ {
			pb, okB := lc.positions[names[b]]
			if !okB {
				continue
			}
			dot := pa[0]*pb[0] + pa[1]*pb[1] + pa[2]*pb[2]
			norm := math.Sqrt(pa[0]*pa[0]+pa[1]*pa[1]+pa[2]*pa[2]) * math.Sqrt(pb[0]*pb[0]+pb[1]*pb[1]+pb[2]*pb[2])
			if norm == 0 || dot/norm > minCos { // cos above minCos means the angle is below the minimum
				continue
			}
			pairs = append(pairs, [2]int{a, b})
		}
	}
	return pairs
}

// computeLinks returns the strongest maxLinks pairs by absolute Pearson
// correlation. windows holds each channel's window back to back
// (channels x win, in time order). Pairs touching a channel whose quality is
// not OK, or with zero variance, are skipped, so the output never holds NaN
// or Inf. centered is scratch space of the same size as windows.
// Called by: session.compute.
func computeLinks(windows []float32, win int, quality []neurov1.Quality, pairs [][2]int, centered []float64) []*neurov1.Link {
	start := time.Now()
	defer func() { linksComputeSeconds.Observe(time.Since(start).Seconds()) }()

	channels := len(quality)
	norms := make([]float64, channels) // 0 means "do not use this channel"
	for c := 0; c < channels; c++ {
		if quality[c] != neurov1.Quality_QUALITY_OK {
			continue
		}
		x := windows[c*win : (c+1)*win]
		var mean float64
		for _, v := range x {
			mean += float64(v)
		}
		mean /= float64(win)
		var ss float64
		out := centered[c*win : (c+1)*win]
		for i, v := range x {
			d := float64(v) - mean
			out[i] = d
			ss += d * d
		}
		if n := math.Sqrt(ss); n > 0 && !math.IsInf(n, 0) && !math.IsNaN(n) {
			norms[c] = n
		}
	}

	var links []*neurov1.Link
	for _, p := range pairs {
		a, b := p[0], p[1]
		if norms[a] == 0 || norms[b] == 0 {
			continue
		}
		xa, xb := centered[a*win:(a+1)*win], centered[b*win:(b+1)*win]
		var sum float64
		for i := range xa {
			sum += xa[i] * xb[i]
		}
		r := math.Abs(sum / (norms[a] * norms[b]))
		if math.IsNaN(r) || math.IsInf(r, 0) {
			continue
		}
		links = append(links, &neurov1.Link{A: uint32(a), B: uint32(b), Strength: float32(min(r, 1))})
	}

	sort.Slice(links, func(i, j int) bool {
		if links[i].Strength != links[j].Strength {
			return links[i].Strength > links[j].Strength
		}
		if links[i].A != links[j].A {
			return links[i].A < links[j].A
		}
		return links[i].B < links[j].B
	})
	if len(links) > maxLinks {
		links = links[:maxLinks]
	}
	return links
}
