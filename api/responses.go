package main

import (
	"context"
	"log"
	"net/http"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/twmb/franz-go/pkg/kgo"
	"google.golang.org/protobuf/proto"

	"github.com/neurohalo/neurohalo/internal/eeg"

	neurov1 "github.com/neurohalo/neurohalo/gen/neuro/v1"
)

// Epochs are the signal around each marker, computed by the processor
// (processor/epochs.go). This file stores them and serves their average per
// marker label: "what does the brain do after a T1 cue, on average?"

// handleEpoch queues one epoch to be written at the end of this poll. An
// epoch for an unknown session, or whose sizes do not fit the session's
// channels, is skipped.
// Used by: store.handle.
func (s *store) handleEpoch(ctx context.Context, rec *kgo.Record) error {
	var e neurov1.Epoch
	if err := proto.Unmarshal(rec.Value, &e); err != nil {
		log.Printf("%s/%d@%d: not an Epoch, skipped: %v", rec.Topic, rec.Partition, rec.Offset, err)
		return nil
	}
	m, err := s.session(ctx, e.SessionId)
	if err != nil || m == nil {
		return err
	}
	if len(e.Bad) != len(m.names) || len(e.BandPower) != int(e.Steps)*len(m.names)*eeg.NumBands ||
		e.PreSteps == 0 || e.PreSteps >= e.Steps {
		return nil
	}
	s.epochs = append(s.epochs, &e)
	return nil
}

// writeEpochs writes this poll's epochs in one round trip. A repeated epoch
// (a processor recomputing after a restart) changes nothing, thanks to the
// primary key and ON CONFLICT DO NOTHING.
// Used by: store.run.
func (s *store) writeEpochs(ctx context.Context) error {
	if len(s.epochs) == 0 {
		return nil
	}
	err := retry(ctx, "write epochs", func(ctx context.Context) error {
		var batch pgx.Batch
		for _, e := range s.epochs {
			batch.Queue(`
				INSERT INTO epochs (session_id, marker_sample, label, at, steps, pre_steps, step_seconds, window_seconds, band_power, bad)
				SELECT $1::text, $2::bigint, $3::text, $4::timestamptz, $5::integer, $6::integer, $7::real, $8::real, $9::real[], $10::boolean[]
				WHERE EXISTS (SELECT 1 FROM sessions WHERE id = $1) -- skipped when the session was deleted meanwhile
				ON CONFLICT DO NOTHING`,
				e.SessionId, int64(e.MarkerSample), e.Label, time.Unix(0, e.TimeUnixNanos),
				int32(e.Steps), int32(e.PreSteps), e.StepSeconds, e.WindowSeconds, e.BandPower, e.Bad)
		}
		return s.db.SendBatch(ctx, &batch).Close()
	})
	if err != nil {
		return err
	}
	s.epochs = s.epochs[:0]
	return nil
}

// epochRow is one stored epoch, as much of it as averaging needs.
// Read by: api.listResponses. Used by: averageResponses.
type epochRow struct {
	label     string
	bandPower []float32 // steps x channels x bands
	bad       []bool    // per channel
}

// responseJSON is the averaged response to one marker label, for one band.
// Built by: averageResponses. Used by: responsesJSON.
type responseJSON struct {
	Label  string `json:"label"`
	Trials int    `json:"trials"` // epochs with this label
	// Per channel, how many of those went into the average: an epoch where
	// the channel was flat or noisy is left out for that channel.
	Good []int `json:"good"`
	// steps x channels, row by row: the band's mean power at each step. The
	// unit is arbitrary; compare a step with the steps before the marker.
	Power []float32 `json:"power"`
}

// responsesJSON is the answer of GET /sessions/{id}/responses.
// Built by: api.listResponses.
type responsesJSON struct {
	Band          string         `json:"band"`
	Times         []float64      `json:"times"`         // per step: the middle of its window, in seconds after the marker
	PreSteps      int            `json:"preSteps"`      // the first this-many steps hold only signal from before the marker
	StepSeconds   float64        `json:"stepSeconds"`   // time between two steps
	WindowSeconds float64        `json:"windowSeconds"` // length of one step's window
	Labels        []responseJSON `json:"labels"`        // sorted by label
}

// averageResponses averages the epochs per label for one band. For each
// channel it leaves out the epochs where that channel was bad. All rows must
// have steps x channels x bands values.
// Used by: api.listResponses.
func averageResponses(rows []epochRow, steps, channels, band int) []responseJSON {
	type sums struct {
		trials int
		good   []int
		power  []float64 // steps x channels
	}
	byLabel := map[string]*sums{}
	for _, row := range rows {
		a := byLabel[row.label]
		if a == nil {
			a = &sums{good: make([]int, channels), power: make([]float64, steps*channels)}
			byLabel[row.label] = a
		}
		a.trials++
		for c := 0; c < channels; c++ {
			if row.bad[c] {
				continue
			}
			a.good[c]++
			for s := 0; s < steps; s++ {
				a.power[s*channels+c] += float64(row.bandPower[(s*channels+c)*eeg.NumBands+band])
			}
		}
	}
	out := []responseJSON{}
	for label, a := range byLabel {
		r := responseJSON{Label: label, Trials: a.trials, Good: a.good, Power: make([]float32, steps*channels)}
		for i, v := range a.power {
			if n := a.good[i%channels]; n > 0 {
				r.Power[i] = float32(v / float64(n))
			}
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Label < out[j].Label })
	return out
}

// listResponses handles GET /sessions/{id}/responses?band=alpha: the averaged
// response to each marker label in one band (alpha when left out), from the
// epochs stored so far. While a session is live the answer grows as trials
// finish. A session without markers gets an empty list of labels.
func (a *api) listResponses(w http.ResponseWriter, r *http.Request) {
	band := -1
	name := r.URL.Query().Get("band")
	if name == "" {
		name = "alpha"
	}
	for i, b := range eeg.Bands {
		if b.Name == name {
			band = i
		}
	}
	if band < 0 {
		writeError(w, http.StatusBadRequest, "band must be delta, theta, alpha, beta or gamma")
		return
	}
	id := r.PathValue("id")
	if !a.sessionExists(w, r, id) {
		return
	}

	rows, err := a.db.Query(r.Context(), `
		SELECT label, steps, pre_steps, step_seconds, window_seconds, band_power, bad
		FROM epochs WHERE session_id = $1 ORDER BY marker_sample`, id)
	if err != nil {
		serverError(w, r, err)
		return
	}
	defer rows.Close()
	out := responsesJSON{Band: name, Times: []float64{}, Labels: []responseJSON{}}
	var epochs []epochRow
	var steps, channels int
	for rows.Next() {
		var e epochRow
		var rowSteps, preSteps int
		var stepSeconds, windowSeconds float32
		if err := rows.Scan(&e.label, &rowSteps, &preSteps, &stepSeconds, &windowSeconds, &e.bandPower, &e.bad); err != nil {
			serverError(w, r, err)
			return
		}
		if len(epochs) == 0 {
			// The first epoch sets the shape; every epoch of a session has the same.
			steps, channels = rowSteps, len(e.bad)
			out.PreSteps, out.StepSeconds, out.WindowSeconds = preSteps, float64(stepSeconds), float64(windowSeconds)
		}
		if rowSteps != steps || len(e.bad) != channels || len(e.bandPower) != steps*channels*eeg.NumBands {
			continue
		}
		epochs = append(epochs, e)
	}
	if err := rows.Err(); err != nil {
		serverError(w, r, err)
		return
	}
	for s := 0; s < steps; s++ {
		end := float64(s-(out.PreSteps-1)) * out.StepSeconds
		out.Times = append(out.Times, end-out.WindowSeconds/2)
	}
	if len(epochs) > 0 {
		out.Labels = averageResponses(epochs, steps, channels, band)
	}
	writeJSON(w, http.StatusOK, out)
}
