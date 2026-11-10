package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/neurohalo/neurohalo/internal/eeg"
)

// api serves session history over REST.
// Created by: main.
// Used by: routes, requireToken, listSessions, getSession, listEvents,
// listRollups, sessionExists (its methods).
type api struct {
	db    *pgxpool.Pool
	token string
}

// sessionJSON is one session as the REST endpoints return it.
// Filled by: scanSession.
// Used by: listSessions, getSession.
type sessionJSON struct {
	ID           string     `json:"id"`
	Source       string     `json:"source"`
	SampleRateHz float64    `json:"sampleRateHz"`
	ChannelNames []string   `json:"channelNames"`
	StartedAt    time.Time  `json:"startedAt"`
	EndedAt      *time.Time `json:"endedAt"`
	Status       string     `json:"status"`
}

// eventJSON is one stored event.
// Used by: listEvents.
type eventJSON struct {
	Type    string          `json:"type"`
	Channel string          `json:"channel"`
	At      time.Time       `json:"at"`
	Details json.RawMessage `json:"details"`
}

// rollupJSON is one 10-second rollup row.
// Used by: listRollups.
type rollupJSON struct {
	BucketStart time.Time `json:"bucketStart"`
	BandPower   []float32 `json:"bandPower"` // channels x bands, row by row
	BadChannels []string  `json:"badChannels"`
	Frames      int       `json:"frames"`
}

const sessionColumns = `id, source, sample_rate_hz, channel_names, started_at, ended_at, status`

// routes returns the REST handler, with every path behind the bearer token.
func (a *api) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /sessions", a.listSessions)
	mux.HandleFunc("GET /sessions/{id}", a.getSession)
	mux.HandleFunc("GET /sessions/{id}/events", a.listEvents)
	mux.HandleFunc("GET /sessions/{id}/rollups", a.listRollups)
	return a.requireToken(mux)
}

// requireToken rejects a request without "Authorization: Bearer <token>".
func (a *api) requireToken(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || subtle.ConstantTimeCompare([]byte(got), []byte(a.token)) != 1 {
			writeError(w, http.StatusUnauthorized, "missing or wrong bearer token")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// writeJSON sends v as a JSON response with the given status.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("write response: %v", err)
	}
}

// writeError sends {"error": msg} with the given status.
func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// serverError logs err and answers 500 without exposing the details.
func serverError(w http.ResponseWriter, r *http.Request, err error) {
	log.Printf("%s %s: %v", r.Method, r.URL.Path, err)
	writeError(w, http.StatusInternalServerError, "internal error")
}

// scanSession reads one row selected with sessionColumns.
func scanSession(row pgx.Row) (sessionJSON, error) {
	var s sessionJSON
	err := row.Scan(&s.ID, &s.Source, &s.SampleRateHz, &s.ChannelNames, &s.StartedAt, &s.EndedAt, &s.Status)
	return s, err
}

// sessionExists answers 404 and returns false when the session is unknown.
func (a *api) sessionExists(w http.ResponseWriter, r *http.Request, id string) bool {
	var exists bool
	if err := a.db.QueryRow(r.Context(), `SELECT EXISTS (SELECT 1 FROM sessions WHERE id = $1)`, id).Scan(&exists); err != nil {
		serverError(w, r, err)
		return false
	}
	if !exists {
		writeError(w, http.StatusNotFound, "no such session")
	}
	return exists
}

// listSessions handles GET /sessions: the newest 100 sessions, newest first.
func (a *api) listSessions(w http.ResponseWriter, r *http.Request) {
	rows, err := a.db.Query(r.Context(), `SELECT `+sessionColumns+` FROM sessions ORDER BY started_at DESC LIMIT 100`)
	if err != nil {
		serverError(w, r, err)
		return
	}
	out := []sessionJSON{}
	for rows.Next() {
		s, err := scanSession(rows)
		if err != nil {
			serverError(w, r, err)
			return
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		serverError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// getSession handles GET /sessions/{id}: one session with its channel names
// and sample rate.
func (a *api) getSession(w http.ResponseWriter, r *http.Request) {
	s, err := scanSession(a.db.QueryRow(r.Context(),
		`SELECT `+sessionColumns+` FROM sessions WHERE id = $1`, r.PathValue("id")))
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		writeError(w, http.StatusNotFound, "no such session")
	case err != nil:
		serverError(w, r, err)
	default:
		writeJSON(w, http.StatusOK, s)
	}
}

// listEvents handles GET /sessions/{id}/events: the session's events in time order.
func (a *api) listEvents(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !a.sessionExists(w, r, id) {
		return
	}
	rows, err := a.db.Query(r.Context(),
		`SELECT type, channel, at, details::text FROM events WHERE session_id = $1 ORDER BY at, id`, id)
	if err != nil {
		serverError(w, r, err)
		return
	}
	out := []eventJSON{}
	for rows.Next() {
		var e eventJSON
		var details string
		if err := rows.Scan(&e.Type, &e.Channel, &e.At, &details); err != nil {
			serverError(w, r, err)
			return
		}
		e.Details = json.RawMessage(details)
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		serverError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// parseTime reads an optional RFC 3339 query parameter. It returns nil when
// the parameter is absent and ok=false (after answering 400) when it is invalid.
func parseTime(w http.ResponseWriter, r *http.Request, name string) (t *time.Time, ok bool) {
	v := r.URL.Query().Get(name)
	if v == "" {
		return nil, true
	}
	parsed, err := time.Parse(time.RFC3339, v)
	if err != nil {
		writeError(w, http.StatusBadRequest, name+": want an RFC 3339 time, e.g. 2026-11-10T20:00:00Z")
		return nil, false
	}
	return &parsed, true
}

// listRollups handles GET /sessions/{id}/rollups?from=&to=: rollup rows with
// from <= bucket_start < to, both optional, plus the band names that explain
// the order inside bandPower.
func (a *api) listRollups(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	from, ok := parseTime(w, r, "from")
	if !ok {
		return
	}
	to, ok := parseTime(w, r, "to")
	if !ok {
		return
	}
	if !a.sessionExists(w, r, id) {
		return
	}
	rows, err := a.db.Query(r.Context(), `
		SELECT bucket_start, band_power, bad_channels, frames FROM rollups
		WHERE session_id = $1
		  AND ($2::timestamptz IS NULL OR bucket_start >= $2)
		  AND ($3::timestamptz IS NULL OR bucket_start < $3)
		ORDER BY bucket_start`, id, from, to)
	if err != nil {
		serverError(w, r, err)
		return
	}
	out := []rollupJSON{}
	for rows.Next() {
		var row rollupJSON
		if err := rows.Scan(&row.BucketStart, &row.BandPower, &row.BadChannels, &row.Frames); err != nil {
			serverError(w, r, err)
			return
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		serverError(w, r, err)
		return
	}
	bands := make([]string, len(eeg.Bands))
	for i, b := range eeg.Bands {
		bands[i] = b.Name
	}
	writeJSON(w, http.StatusOK, map[string]any{"bands": bands, "rollups": out})
}

// shutdown stops the HTTP server, giving open requests a few seconds.
func shutdown(srv *http.Server) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
}
