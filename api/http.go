package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/minio/minio-go/v7"

	"github.com/neurohalo/neurohalo/internal/eeg"
)

// api serves session history over REST, live frames over WebSocket, and
// starts replays of samples and uploads.
// Created by: main.
// Used by: routes, cors, requireToken, listSessions, getSession, listEvents,
// listRollups, sessionExists (http.go); listResponses (responses.go); liveSession (live.go); listSamples,
// playSample, upload, getReplay, queueReplay (uploads.go); getRecording, removeRecording (recordings.go);
// listStudies, createStudy, getStudy, deleteStudy, setSessionStudy, setParticipant (studies.go); exportBIDS (bids.go);
// liveOverview (overview.go); getFacts, getSummary, createSummary (summary.go).
type api struct {
	db        *pgxpool.Pool
	token     string
	hub       *hub
	objects   *objectStore // nil when uploads are not configured
	origins   []string     // web app origins allowed by CORS and the WebSocket
	wsClients atomic.Int64
	summarizer summarizer // writes session summaries (Claude)
}

// sessionJSON is one session as the REST endpoints return it.
// Filled by: scanSession.
// Used by: listSessions, getSession, getStudy (studies.go), liveOverview (overview.go), sessionFacts (summary.go).
type sessionJSON struct {
	ID           string     `json:"id"`
	Label        string     `json:"label"` // sample title or uploaded file name; empty for hand-run replays
	Source       string     `json:"source"`
	SampleRateHz float64    `json:"sampleRateHz"`
	ChannelNames []string   `json:"channelNames"`
	StartedAt    time.Time  `json:"startedAt"`
	EndedAt      *time.Time `json:"endedAt"`
	Status       string     `json:"status"`
	// The recorder's file of the raw signal; nil before its first chunk is stored.
	Recording *recordingJSON `json:"recording"`
	// The study the session belongs to ("" for none), and inside it who was
	// recorded and what they did. Set by PUT /sessions/{id}/study.
	StudyID     string `json:"studyId"`
	Participant string `json:"participant"`
	Task        string `json:"task"`
	Visit       string `json:"visit"` // "" unless the participant is recorded on several visits
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

// sessionColumns and sessionFrom read sessions with the label of the replay
// that started them, the state of their recording and their study tags, in
// the order scanSession expects. sessionSelect is the two together; the live
// overview adds its own columns and joins between them.
const (
	sessionColumns = `s.id, COALESCE(r.label, ''), s.source, s.sample_rate_hz, s.channel_names,
	s.started_at, s.ended_at, s.status, rec.status, rec.bytes, rec.seconds,
	COALESCE(s.study_id, ''), s.participant, s.task, s.visit`
	sessionFrom = `
	FROM sessions s LEFT JOIN replays r ON r.session_id = s.id
	LEFT JOIN recordings rec ON rec.session_id = s.id`
	sessionSelect = `SELECT ` + sessionColumns + sessionFrom
)

// routes returns the HTTP handler: CORS outside, then the bearer token, then
// the endpoints.
func (a *api) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /sessions", a.listSessions)
	mux.HandleFunc("GET /live", a.liveOverview)
	mux.HandleFunc("GET /sessions/{id}", a.getSession)
	mux.HandleFunc("DELETE /sessions/{id}", a.deleteSession)
	mux.HandleFunc("GET /sessions/{id}/events", a.listEvents)
	mux.HandleFunc("GET /sessions/{id}/facts", a.getFacts)
	mux.HandleFunc("GET /sessions/{id}/summary", a.getSummary)
	mux.HandleFunc("POST /sessions/{id}/summary", a.createSummary)
	mux.HandleFunc("GET /sessions/{id}/rollups", a.listRollups)
	mux.HandleFunc("GET /sessions/{id}/responses", a.listResponses)
	mux.HandleFunc("GET /sessions/{id}/erp", a.getERP)
	mux.HandleFunc("GET /sessions/{id}/recording.edf", a.getRecording)
	mux.HandleFunc("GET /ws/sessions/{id}/live", a.liveSession)
	mux.HandleFunc("GET /samples", a.listSamples)
	mux.HandleFunc("POST /samples/{name}/play", a.playSample)
	mux.HandleFunc("POST /uploads", a.upload)
	mux.HandleFunc("GET /replays/{id}", a.getReplay)
	mux.HandleFunc("POST /sessions/{id}/replay", a.replayFrom)
	mux.HandleFunc("PUT /sessions/{id}/study", a.setSessionStudy)
	mux.HandleFunc("PUT /studies/{id}/participants/{label}", a.setParticipant)
	mux.HandleFunc("GET /studies", a.listStudies)
	mux.HandleFunc("POST /studies", a.createStudy)
	mux.HandleFunc("GET /studies/{id}", a.getStudy)
	mux.HandleFunc("DELETE /studies/{id}", a.deleteStudy)
	mux.HandleFunc("GET /studies/{id}/bids.zip", a.exportBIDS)
	return a.cors(a.requireToken(mux))
}

// cors lets the web app, served from another origin, call the api: it answers
// preflight requests and marks responses for allowed origins.
func (a *api) cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		for _, o := range a.origins {
			if origin == o {
				w.Header().Set("Access-Control-Allow-Origin", origin)
				w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
				w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE")
				w.Header().Set("Access-Control-Max-Age", "600")
				w.Header().Add("Vary", "Origin")
				break
			}
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// requireToken rejects a request without "Authorization: Bearer <token>".
// Browsers cannot set headers on a WebSocket or on a plain download link, so
// /ws/ paths and file downloads (.edf, .zip) may pass the token as ?token= instead.
func (a *api) requireToken(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok && (strings.HasPrefix(r.URL.Path, "/ws/") || isDownload(r.URL.Path)) {
			got, ok = r.URL.Query().Get("token"), true
		}
		if !ok || subtle.ConstantTimeCompare([]byte(got), []byte(a.token)) != 1 {
			writeError(w, http.StatusUnauthorized, "missing or wrong bearer token")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// isDownload reports whether a path is a file a browser fetches with a plain
// link: a session's EDF file or a study's BIDS zip.
// Used by: api.requireToken.
func isDownload(path string) bool {
	return strings.HasSuffix(path, "/recording.edf") || strings.HasSuffix(path, "/bids.zip")
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

// scanSession reads one row selected with sessionColumns, then any extra
// columns after them into extra.
func scanSession(row pgx.Row, extra ...any) (sessionJSON, error) {
	var s sessionJSON
	var recStatus *string
	var recBytes *int64
	var recSeconds *float64
	err := row.Scan(append([]any{&s.ID, &s.Label, &s.Source, &s.SampleRateHz, &s.ChannelNames, &s.StartedAt, &s.EndedAt, &s.Status,
		&recStatus, &recBytes, &recSeconds, &s.StudyID, &s.Participant, &s.Task, &s.Visit}, extra...)...)
	if err == nil && recStatus != nil {
		s.Recording = &recordingJSON{Status: *recStatus, Bytes: *recBytes, Seconds: *recSeconds}
	}
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
// Load-test sessions (source "loadgen") are left out, so a test with 1,000
// of them does not bury the real ones; each is still reachable by its ID.
func (a *api) listSessions(w http.ResponseWriter, r *http.Request) {
	rows, err := a.db.Query(r.Context(),
		sessionSelect+` WHERE s.source <> 'loadgen' ORDER BY s.started_at DESC LIMIT 100`)
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
	s, err := scanSession(a.db.QueryRow(r.Context(), sessionSelect+` WHERE s.id = $1`, r.PathValue("id")))
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		writeError(w, http.StatusNotFound, "no such session")
	case err != nil:
		serverError(w, r, err)
	default:
		writeJSON(w, http.StatusOK, s)
	}
}

// deleteSession handles DELETE /sessions/{id}: removes an ended session with
// its events, rollups, epochs, summary, replay job and recording, all in one
// transaction. A live session is refused (409): its stream is still writing.
// The recording's files are removed after the commit, and so is the uploaded
// file behind the session when no other session uses it.
func (a *api) deleteSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ctx := r.Context()
	tx, err := a.db.Begin(ctx)
	if err != nil {
		serverError(w, r, err)
		return
	}
	defer tx.Rollback(ctx) // does nothing after a commit

	var status string
	err = tx.QueryRow(ctx, `SELECT status FROM sessions WHERE id = $1 FOR UPDATE`, id).Scan(&status)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		writeError(w, http.StatusNotFound, "no such session")
		return
	case err != nil:
		serverError(w, r, err)
		return
	case status == "live":
		writeError(w, http.StatusConflict, "the session is still live; wait until it has ended")
		return
	}

	// The upload behind the session, if this is the last session that uses it.
	var orphan string
	err = tx.QueryRow(ctx, `
		SELECT object_key FROM replays r
		WHERE session_id = $1 AND kind = 'upload' AND object_key <> ''
		  AND NOT EXISTS (SELECT 1 FROM replays o WHERE o.object_key = r.object_key AND o.session_id <> r.session_id)`,
		id).Scan(&orphan)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		serverError(w, r, err)
		return
	}

	for _, q := range []string{
		`DELETE FROM events WHERE session_id = $1`,
		`DELETE FROM rollups WHERE session_id = $1`,
		`DELETE FROM epochs WHERE session_id = $1`,
		`DELETE FROM summaries WHERE session_id = $1`,
		`DELETE FROM replays WHERE session_id = $1`,
		`DELETE FROM recordings WHERE session_id = $1`,
		`DELETE FROM sessions WHERE id = $1`,
	} {
		if _, err := tx.Exec(ctx, q, id); err != nil {
			serverError(w, r, err)
			return
		}
	}
	if err := tx.Commit(ctx); err != nil {
		serverError(w, r, err)
		return
	}
	// The rows are gone; a file left behind only wastes space, so a failure here is logged, not returned.
	if a.objects != nil {
		if orphan != "" {
			if err := a.objects.client.RemoveObject(ctx, a.objects.bucket, orphan, minio.RemoveObjectOptions{}); err != nil {
				log.Printf("delete session %s: cannot remove %s: %v", id, orphan, err)
			}
		}
		a.removeRecording(ctx, id)
	}
	writeJSON(w, http.StatusOK, map[string]string{"deleted": id})
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
