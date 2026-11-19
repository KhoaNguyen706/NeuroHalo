package main

import (
	"context"
	"errors"
	"math"
	"net/http"
	"sort"
	"strconv"

	"github.com/jackc/pgx/v5"
)

// The ERP (event-related potential) is the voltage averaged over the trials
// of one marker label: a few microvolts of response to a cue that only show
// once the rest of the signal, which is not locked to the cue, averages out.
//
// The processor keeps the raw signal from 0.5 s before to 1.3 s after each
// marker (processor/epochs.go). Per trial and sensor this file does what an
// ERP pipeline in MNE or ERPLAB does, in that order:
//  1. low-pass at 30 Hz with the same filter MNE designs by default, so
//     mains hum and muscle noise do not swamp the average;
//  2. keep 0.2 s before to 0.8 s after the marker; the margins the filter
//     needed are dropped, so the result equals filtering the whole recording;
//  3. subtract the mean of the 0.2 s before the marker (baseline), so every
//     trial starts from 0 µV;
//  4. leave the trial out for this sensor when its peak-to-peak is above the
//     rejection threshold (a blink, a movement) or below 1 µV (flat).
// No high-pass filter is applied: the baseline takes the slow drift out
// within each 1-second trial.
//
// Next to the average, it reports how precise the average is: the SME
// (standardized measurement error, Luck et al. 2021) of the mean voltage in
// each 100 ms window, as ERPLAB computes it by default (its make_DQ_spec and
// sme_analytic): windows lined up on the marker, each taking every sample from
// its start to its end inclusive, so a sample on a boundary counts in both
// windows next to it. The SME is the standard
// error of that bin's mean voltage across the kept trials (their standard
// deviation over the square root of their number): the smaller it is, the
// less the average would move with other trials. It falls with more trials
// and rises with noisier ones, so it is a recording's data quality in µV.

const (
	erpLowpassHz     = 30.0
	erpBeforeSeconds = 0.2
	erpAfterSeconds  = 0.8
	erpFlatMicrovolt = 1.0   // peak-to-peak below this: the sensor is flat in that trial
	erpRejectDefault = 100.0 // µV peak-to-peak; the usual starting point in ERP labs
	erpBinSeconds    = 0.1   // the SME's time bins, ERPLAB's default
)

// lowpassKernel designs the low-pass filter MNE's filter_data uses by default
// (FIR, "firwin" design, Hamming window, zero phase): the transition band is
// a quarter of the cutoff, at least 2 Hz, the -6 dB point sits in its middle,
// and the length is 3.3 / transition × rate, rounded and made odd. The taps
// are scipy's firwin: a windowed sinc, scaled to a gain of exactly 1 at 0 Hz.
// Returns nil when the cutoff is at or above the Nyquist frequency.
// Used by: getERP.
func lowpassKernel(rate, cutoff float64) []float64 {
	nyquist := rate / 2
	trans := math.Min(math.Max(0.25*cutoff, 2), nyquist-cutoff)
	if trans <= 0 {
		return nil
	}
	n := int(math.Round(3.3 / trans * rate))
	n += 1 - n%2
	edge := (cutoff + trans/2) / nyquist // the -6 dB point, as a fraction of Nyquist
	h := make([]float64, n)
	var sum float64
	for i := range h {
		m := float64(i) - float64(n-1)/2
		sinc := 1.0
		if m != 0 {
			sinc = math.Sin(math.Pi*edge*m) / (math.Pi * edge * m)
		}
		window := 1.0
		if n > 1 {
			window = 0.54 - 0.46*math.Cos(2*math.Pi*float64(i)/float64(n-1))
		}
		h[i] = edge * sinc * window
		sum += h[i]
	}
	for i := range h {
		h[i] /= sum
	}
	return h
}

// erpShape is where the ERP window sits inside a stored raw stretch, and the
// filter applied before cutting it out.
// Built by: getERP. Used by: erpTrial, averageERP.
type erpShape struct {
	channels int
	pre      int       // the marker's row in the stored stretch
	from, to int       // the ERP's rows in the stretch, to inclusive
	baseline int       // rows from `from` up to and including the marker
	kernel   []float64 // odd length, symmetric
	// The SME windows: per window, its first and last ERP sample, inclusive.
	binFirst, binLast []int
}

// erpTrial filters, cuts and baselines one sensor of one stored trial into
// out (to-from+1 values) and returns its peak-to-peak in µV. The stretch must
// reach at least half the kernel past both ends of the ERP window.
// Used by: averageERP.
func erpTrial(voltage []float32, c int, sh erpShape, out []float64) float64 {
	half := len(sh.kernel) / 2
	var base float64
	for i := range out {
		row := sh.from + i
		var v float64
		for k, h := range sh.kernel {
			v += h * float64(voltage[(row+k-half)*sh.channels+c])
		}
		out[i] = v
		if i < sh.baseline {
			base += v
		}
	}
	base /= float64(sh.baseline)
	lo, hi := math.Inf(1), math.Inf(-1)
	for i := range out {
		out[i] -= base
		lo, hi = math.Min(lo, out[i]), math.Max(hi, out[i])
	}
	return hi - lo
}

// voltageRow is one stored trial's raw stretch.
// Read by: getERP. Used by: averageERP.
type voltageRow struct {
	label   string
	voltage []float32 // samples x channels
}

// erpLabelJSON is the ERP of one marker label.
// Built by: averageERP. Used by: erpJSON.
type erpLabelJSON struct {
	Label  string `json:"label"`
	Trials int    `json:"trials"` // trials with this label
	// Per sensor, how many of them went into its average: the others were
	// over the rejection threshold or flat.
	Good []int `json:"good"`
	// samples x channels, row by row: the mean voltage in µV, 0 for a sensor
	// with no good trial.
	Mean []float32 `json:"mean"`
	// bins x channels, row by row: the SME of each bin's mean voltage in µV,
	// -1 for a sensor with fewer than 2 good trials.
	SME []float32 `json:"sme"`
}

// erpJSON is the answer of GET /sessions/{id}/erp.
// Built by: getERP.
type erpJSON struct {
	Times     []float64      `json:"times"`     // per sample: seconds after the marker
	LowpassHz float64        `json:"lowpassHz"` // the filter applied first
	Reject    float64        `json:"reject"`    // µV peak-to-peak; 0 when nothing is rejected
	Bins      []float64      `json:"bins"`      // per SME bin: its start, in seconds after the marker
	Labels    []erpLabelJSON `json:"labels"`    // sorted by label
}

// averageERP turns every trial into its ERP window per sensor and averages
// them per label, leaving out per sensor the trials that are flat or, when
// reject is above 0, whose peak-to-peak is above reject. It also gives the
// SME of every bin from the same kept trials. Every row must hold a full
// stretch of the shape's size.
// Used by: getERP.
func averageERP(rows []voltageRow, sh erpShape, reject float64) []erpLabelJSON {
	samples := sh.to - sh.from + 1
	type sums struct {
		trials  int
		good    []int
		total   []float64 // samples x channels
		binSum  []float64 // bins x channels: the sum of the trials' bin means
		binSum2 []float64 // bins x channels: the sum of their squares
	}
	byLabel := map[string]*sums{}
	trial := make([]float64, samples)
	numBins := len(sh.binFirst)
	for _, row := range rows {
		a := byLabel[row.label]
		if a == nil {
			a = &sums{
				good: make([]int, sh.channels), total: make([]float64, samples*sh.channels),
				binSum: make([]float64, numBins*sh.channels), binSum2: make([]float64, numBins*sh.channels),
			}
			byLabel[row.label] = a
		}
		a.trials++
		for c := 0; c < sh.channels; c++ {
			span := erpTrial(row.voltage, c, sh, trial)
			if span < erpFlatMicrovolt || (reject > 0 && span > reject) {
				continue
			}
			a.good[c]++
			for i, v := range trial {
				a.total[i*sh.channels+c] += v
			}
			for b := range numBins {
				var v float64
				for i := sh.binFirst[b]; i <= sh.binLast[b]; i++ {
					v += trial[i]
				}
				v /= float64(sh.binLast[b] - sh.binFirst[b] + 1)
				a.binSum[b*sh.channels+c] += v
				a.binSum2[b*sh.channels+c] += v * v
			}
		}
	}
	out := []erpLabelJSON{}
	for label, a := range byLabel {
		l := erpLabelJSON{
			Label: label, Trials: a.trials, Good: a.good,
			Mean: make([]float32, len(a.total)), SME: make([]float32, len(a.binSum)),
		}
		for i, v := range a.total {
			if n := a.good[i%sh.channels]; n > 0 {
				l.Mean[i] = float32(v / float64(n))
			}
		}
		for i, sum := range a.binSum {
			n := float64(a.good[i%sh.channels])
			if n < 2 {
				l.SME[i] = -1
				continue
			}
			variance := max(0, (a.binSum2[i]-sum*sum/n)/(n-1)) // sample variance of the trials' bin means
			l.SME[i] = float32(math.Sqrt(variance / n))
		}
		out = append(out, l)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Label < out[j].Label })
	return out
}

// smeWindows lays out the SME windows the way ERPLAB's make_DQ_spec does by
// default: 100 ms long, starting at the marker and every 100 ms before and
// after it within the ERP; if the last one runs past the end, it is dropped.
// Each window's start and end are then the samples nearest to those times
// (the first, on a tie), as in ERPLAB's sme_analytic, and both are included.
// Returns each window's start in seconds and its first and last sample.
// Used by: getERP.
func smeWindows(times []float64) (starts []float64, first, last []int) {
	const eps = 1e-9
	lo, hi := times[0], times[len(times)-1]
	if lo < 0 && hi > 0 {
		for t := 0.0; t >= lo-eps; t -= erpBinSeconds {
			starts = append([]float64{t}, starts...)
		}
		for t := erpBinSeconds; t <= hi+eps; t += erpBinSeconds {
			starts = append(starts, t)
		}
	} else {
		for t := lo; t <= hi+eps; t += erpBinSeconds {
			starts = append(starts, t)
		}
	}
	if len(starts) > 0 && starts[len(starts)-1]+erpBinSeconds > hi+eps {
		starts = starts[:len(starts)-1]
	}
	nearest := func(t float64) int {
		best := 0
		for i, x := range times {
			if math.Abs(x-t) < math.Abs(times[best]-t)-eps {
				best = i
			}
		}
		return best
	}
	for _, s := range starts {
		first = append(first, nearest(s))
		last = append(last, nearest(s+erpBinSeconds))
	}
	return starts, first, last
}

// getERP handles GET /sessions/{id}/erp?reject=100: the ERP of each marker
// label from the trials stored so far, with trials over `reject` µV
// peak-to-peak left out per sensor (100 when left out, 0 to keep every trial
// that is not flat). While a session is live the answer grows as trials finish.
func (a *api) getERP(w http.ResponseWriter, r *http.Request) {
	reject := erpRejectDefault
	if text := r.URL.Query().Get("reject"); text != "" {
		v, err := strconv.ParseFloat(text, 64)
		if err != nil || v < 0 || math.IsInf(v, 0) {
			writeError(w, http.StatusBadRequest, "reject must be a number of microvolts, 0 or more")
			return
		}
		reject = v
	}
	out, err := a.computeERP(r.Context(), r.PathValue("id"), reject)
	switch {
	case errors.Is(err, errNoSession):
		writeError(w, http.StatusNotFound, "no such session")
	case err != nil:
		serverError(w, r, err)
	default:
		writeJSON(w, http.StatusOK, out)
	}
}

// errNoSession is what computeERP (and sessionFacts) return for an unknown session.
var errNoSession = errors.New("no such session")

// computeERP builds the ERP of each marker label from the trials stored so
// far, with trials over reject µV peak-to-peak left out per sensor (0 keeps
// every trial that is not flat). It returns errNoSession for an unknown session.
// Used by: getERP, sessionFacts (summary.go).
func (a *api) computeERP(ctx context.Context, id string, reject float64) (erpJSON, error) {
	var rate float64
	var names []string
	err := a.db.QueryRow(ctx, `SELECT sample_rate_hz, channel_names FROM sessions WHERE id = $1`, id).Scan(&rate, &names)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return erpJSON{}, errNoSession
	case err != nil:
		return erpJSON{}, err
	}

	out := erpJSON{Times: []float64{}, LowpassHz: erpLowpassHz, Reject: reject, Bins: []float64{}, Labels: []erpLabelJSON{}}
	kernel := lowpassKernel(rate, erpLowpassHz)
	if kernel == nil {
		return out, nil // a rate too low for a 30 Hz filter: no ERP
	}
	rows, err := a.db.Query(ctx, `
		SELECT label, voltage_samples, voltage_pre, voltage
		FROM epochs WHERE session_id = $1 AND voltage_samples > 0 ORDER BY marker_sample`, id)
	if err != nil {
		return erpJSON{}, err
	}
	defer rows.Close()
	var trials []voltageRow
	var sh erpShape
	var samples int
	for rows.Next() {
		var t voltageRow
		var n, pre int
		if err := rows.Scan(&t.label, &n, &pre, &t.voltage); err != nil {
			return erpJSON{}, err
		}
		if len(trials) == 0 {
			// The first trial sets the shape; every trial of a session has the same.
			samples = n
			before := int(math.Round(erpBeforeSeconds * rate))
			sh = erpShape{
				channels: len(names), pre: pre, kernel: kernel,
				from: pre - before, to: pre + int(math.Round(erpAfterSeconds*rate)), baseline: before + 1,
			}
			half := len(kernel) / 2
			if sh.from-half < 0 || sh.to+half >= n {
				break // the stored stretch is too short to filter; cannot happen with the processor's constants
			}
		}
		if n != samples || pre != sh.pre || len(t.voltage) != n*len(names) {
			continue
		}
		trials = append(trials, t)
	}
	if err := rows.Err(); err != nil {
		return erpJSON{}, err
	}
	if len(trials) == 0 {
		return out, nil
	}
	for row := sh.from; row <= sh.to; row++ {
		out.Times = append(out.Times, float64(row-sh.pre)/rate)
	}
	var starts []float64
	starts, sh.binFirst, sh.binLast = smeWindows(out.Times)
	out.Bins = append(out.Bins, starts...)
	out.Labels = averageERP(trials, sh, reject)
	return out, nil
}
