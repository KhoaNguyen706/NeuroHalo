package main

import (
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
// each 100 ms bin, as ERPLAB computes it by default. The SME is the standard
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
	bins     []int     // per ERP sample: its SME bin, from 0
	numBins  int
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
	binMean := make([]float64, sh.numBins)
	binCount := make([]int, sh.numBins)
	for i := range trial {
		binCount[sh.bins[i]]++
	}
	for _, row := range rows {
		a := byLabel[row.label]
		if a == nil {
			a = &sums{
				good: make([]int, sh.channels), total: make([]float64, samples*sh.channels),
				binSum: make([]float64, sh.numBins*sh.channels), binSum2: make([]float64, sh.numBins*sh.channels),
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
			clear(binMean)
			for i, v := range trial {
				a.total[i*sh.channels+c] += v
				binMean[sh.bins[i]] += v
			}
			for b, v := range binMean {
				v /= float64(binCount[b])
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

// binsOf puts each ERP sample time into a 100 ms bin counted from the first
// time; the last sample (exactly at the window's end) joins the last bin.
// Returns each sample's bin and the number of bins.
// Used by: getERP.
func binsOf(times []float64) ([]int, int) {
	n := max(1, int(math.Round((times[len(times)-1]-times[0])/erpBinSeconds)))
	bins := make([]int, len(times))
	for i, t := range times {
		bins[i] = min(int(math.Floor((t-times[0])/erpBinSeconds+1e-9)), n-1)
	}
	return bins, n
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
	id := r.PathValue("id")
	var rate float64
	var names []string
	err := a.db.QueryRow(r.Context(), `SELECT sample_rate_hz, channel_names FROM sessions WHERE id = $1`, id).Scan(&rate, &names)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		writeError(w, http.StatusNotFound, "no such session")
		return
	case err != nil:
		serverError(w, r, err)
		return
	}

	out := erpJSON{Times: []float64{}, LowpassHz: erpLowpassHz, Reject: reject, Bins: []float64{}, Labels: []erpLabelJSON{}}
	kernel := lowpassKernel(rate, erpLowpassHz)
	if kernel == nil {
		writeJSON(w, http.StatusOK, out) // a rate too low for a 30 Hz filter: no ERP
		return
	}
	rows, err := a.db.Query(r.Context(), `
		SELECT label, voltage_samples, voltage_pre, voltage
		FROM epochs WHERE session_id = $1 AND voltage_samples > 0 ORDER BY marker_sample`, id)
	if err != nil {
		serverError(w, r, err)
		return
	}
	defer rows.Close()
	var trials []voltageRow
	var sh erpShape
	var samples int
	for rows.Next() {
		var t voltageRow
		var n, pre int
		if err := rows.Scan(&t.label, &n, &pre, &t.voltage); err != nil {
			serverError(w, r, err)
			return
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
		serverError(w, r, err)
		return
	}
	if len(trials) == 0 {
		writeJSON(w, http.StatusOK, out)
		return
	}
	for row := sh.from; row <= sh.to; row++ {
		out.Times = append(out.Times, float64(row-sh.pre)/rate)
	}
	sh.bins, sh.numBins = binsOf(out.Times)
	for b := 0; b < sh.numBins; b++ {
		out.Bins = append(out.Bins, out.Times[0]+float64(b)*erpBinSeconds)
	}
	out.Labels = averageERP(trials, sh, reject)
	writeJSON(w, http.StatusOK, out)
}
