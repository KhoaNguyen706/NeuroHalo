package main

import (
	"net/http"
	"sync"
	"time"

	"github.com/neurohalo/neurohalo/internal/eeg"
)

// stalledAfter is how long a live session may go without a frame before the
// overview says its data stopped. Frames come every 0.5 s.
const stalledAfter = 3 * time.Second

// lastFrame is when the newest frame of one session reached this api, and
// which window it was.
// Kept by: frameClock. Read by: api.liveOverview.
type lastFrame struct {
	at     time.Time
	window uint64
}

// frameClock remembers the newest frame of every session, as the live
// consumer hands frames to the hub. It is how the overview knows, to the half
// second, whether data is still arriving, without asking the database.
// Kept in: hub.frames (its zero value is ready). Used by: hub.publishFrame (note),
// api.liveOverview (get, forget).
type frameClock struct {
	mu   sync.Mutex
	last map[string]lastFrame
}

// note records a session's newest frame.
func (c *frameClock) note(sessionID string, window uint64, at time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.last == nil {
		c.last = map[string]lastFrame{}
	}
	c.last[sessionID] = lastFrame{at: at, window: window}
}

// get returns a session's newest frame; ok is false when none has arrived
// since this api started.
func (c *frameClock) get(sessionID string) (lastFrame, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	f, ok := c.last[sessionID]
	return f, ok
}

// forget drops every session not in keep, so ended sessions do not pile up.
func (c *frameClock) forget(keep map[string]bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for id := range c.last {
		if !keep[id] {
			delete(c.last, id)
		}
	}
}

// liveRow is one live session on the overview: who is recorded, how long it
// has run, whether data is arriving, and what would make the recording less
// usable: sensors marked bad now, and how often the eyes and muscles got in.
// Built by: api.liveOverview.
type liveRow struct {
	Session sessionJSON `json:"session"`
	Study   string      `json:"study"` // the study's name, "" when not filed
	// Seconds of signal processed so far, on the recording's own clock; 0
	// until this api has seen a frame.
	Seconds float64 `json:"seconds"`
	// Milliseconds since the newest frame arrived; -1 when none has arrived
	// since this api started.
	LastFrameMs int64    `json:"lastFrameMs"`
	Stalled     bool     `json:"stalled"`
	Bad         []string `json:"bad"`    // sensors marked bad now, by name
	Trials      int      `json:"trials"` // markers so far
	Eye         int      `json:"eye"`    // eye movements so far
	Muscle      int      `json:"muscle"` // stretches of muscle activity so far
}

// liveOverviewQuery reads every live session as sessionSelect does, then its
// study's name, its marker, eye and muscle counts, and the sensors whose
// latest channel_bad or channel_recovered event is channel_bad.
const liveOverviewQuery = `SELECT ` + sessionColumns + `,
	COALESCE(st.name, ''), c.trials, c.eye, c.muscle, b.bad` + sessionFrom + `
	LEFT JOIN studies st ON st.id = s.study_id
	CROSS JOIN LATERAL (SELECT
		count(*) FILTER (WHERE type = 'marker') AS trials,
		count(*) FILTER (WHERE type = 'eye_movement') AS eye,
		count(*) FILTER (WHERE type = 'muscle') AS muscle
		FROM events WHERE session_id = s.id) c
	CROSS JOIN LATERAL (SELECT COALESCE(array_agg(channel ORDER BY channel), '{}') AS bad FROM (
		SELECT DISTINCT ON (channel) channel, type FROM events
		WHERE session_id = s.id AND type IN ('channel_bad', 'channel_recovered')
		ORDER BY channel, at DESC, type) latest WHERE type = 'channel_bad') b
	WHERE s.status = 'live' AND s.source <> 'loadgen'
	ORDER BY s.started_at`

// liveOverview handles GET /live: every live session at once, for a lab
// running several rooms. The counts and bad sensors come from the stored
// events (a second or two behind); whether data is arriving comes from the
// live consumer's frames.
func (a *api) liveOverview(w http.ResponseWriter, r *http.Request) {
	rows, err := a.db.Query(r.Context(), liveOverviewQuery)
	if err != nil {
		serverError(w, r, err)
		return
	}
	defer rows.Close()
	now := time.Now()
	out := []liveRow{}
	keep := map[string]bool{}
	for rows.Next() {
		var row liveRow
		var err error
		row.Session, err = scanSession(rows, &row.Study, &row.Trials, &row.Eye, &row.Muscle, &row.Bad)
		if err != nil {
			serverError(w, r, err)
			return
		}
		keep[row.Session.ID] = true
		row.LastFrameMs = -1
		if f, ok := a.hub.frames.get(row.Session.ID); ok {
			age := now.Sub(f.at)
			row.LastFrameMs = age.Milliseconds()
			row.Stalled = age > stalledAfter
			row.Seconds = float64(f.window) * eeg.HopSeconds
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		serverError(w, r, err)
		return
	}
	a.hub.frames.forget(keep)
	writeJSON(w, http.StatusOK, out)
}
