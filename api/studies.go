package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// A study groups sessions so they can be downloaded together as one BIDS
// dataset (bids.go). Inside a study each session is tagged with a participant
// and a task. Sessions of any source can be tagged, live ones too, so a lab
// recording from an amplifier can file the session while it runs.

// bidsLabel is what BIDS allows in a participant or task label: letters and
// digits only. The labels become part of file names (sub-01_task-rest_eeg.edf).
var bidsLabel = regexp.MustCompile(`^[A-Za-z0-9]{1,32}$`)

// maxStudyName is the longest study name accepted, in characters.
const maxStudyName = 100

// studyJSON is one study in the list of studies.
// Built by: listStudies. Also the head of studyDetailJSON.
type studyJSON struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"createdAt"`
	Sessions  int       `json:"sessions"` // how many sessions are in it
}

// studyDetailJSON is one study with its sessions.
// Built by: getStudy.
type studyDetailJSON struct {
	ID           string            `json:"id"`
	Name         string            `json:"name"`
	CreatedAt    time.Time         `json:"createdAt"`
	SessionList  []sessionJSON     `json:"sessionList"`  // by participant, visit, task, then start time
	Participants []participantJSON `json:"participants"` // only those with something entered
}

// participantJSON is what a study knows about one participant, for
// participants.tsv. It is the body of PUT /studies/{id}/participants/{label}
// (without the label) and part of studyDetailJSON.
// Read by: setParticipant, readParticipants. Used by: getStudy, exportBIDS (bids.go).
type participantJSON struct {
	Label string `json:"label"`
	Age   *int   `json:"age"` // in years; null when not given
	Sex   string `json:"sex"` // "M", "F", "O", or "" when not given
}

// maxAge is the oldest age accepted, in years.
const maxAge = 120

// studyTagJSON is the body of PUT /sessions/{id}/study.
// Read by: setSessionStudy.
type studyTagJSON struct {
	StudyID     string `json:"studyId"` // "" takes the session out of its study
	Participant string `json:"participant"`
	Task        string `json:"task"`
	Visit       string `json:"visit"` // optional: "" for none
}

// listStudies handles GET /studies: every study, newest first, with how many
// sessions each holds.
func (a *api) listStudies(w http.ResponseWriter, r *http.Request) {
	rows, err := a.db.Query(r.Context(), `
		SELECT st.id, st.name, st.created_at, COUNT(s.id)
		FROM studies st LEFT JOIN sessions s ON s.study_id = st.id
		GROUP BY st.id ORDER BY st.created_at DESC`)
	if err != nil {
		serverError(w, r, err)
		return
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (studyJSON, error) {
		var s studyJSON
		err := row.Scan(&s.ID, &s.Name, &s.CreatedAt, &s.Sessions)
		return s, err
	})
	if err != nil {
		serverError(w, r, err)
		return
	}
	if out == nil {
		out = []studyJSON{}
	}
	writeJSON(w, http.StatusOK, out)
}

// createStudy handles POST /studies with {"name": "..."}: makes an empty
// study and answers {"id": "st-..."}.
func (a *api) createStudy(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, `the body must be {"name": "..."}`)
		return
	}
	name := strings.TrimSpace(body.Name)
	if name == "" || utf8.RuneCountInString(name) > maxStudyName || !utf8.ValidString(name) {
		writeError(w, http.StatusBadRequest, "the name must be 1 to 100 characters")
		return
	}
	id := newID("st-")
	if _, err := a.db.Exec(r.Context(), `INSERT INTO studies (id, name) VALUES ($1, $2)`, id, name); err != nil {
		serverError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"id": id})
}

// getStudy handles GET /studies/{id}: the study and its sessions, ordered as
// they will be numbered in the BIDS export.
func (a *api) getStudy(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var out studyDetailJSON
	err := a.db.QueryRow(r.Context(), `SELECT id, name, created_at FROM studies WHERE id = $1`, id).
		Scan(&out.ID, &out.Name, &out.CreatedAt)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		writeError(w, http.StatusNotFound, "no such study")
		return
	case err != nil:
		serverError(w, r, err)
		return
	}
	rows, err := a.db.Query(r.Context(),
		sessionSelect+` WHERE s.study_id = $1 ORDER BY s.participant, s.visit, s.task, s.started_at`, id)
	if err != nil {
		serverError(w, r, err)
		return
	}
	out.SessionList, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (sessionJSON, error) {
		return scanSession(row)
	})
	if err != nil {
		serverError(w, r, err)
		return
	}
	if out.SessionList == nil {
		out.SessionList = []sessionJSON{}
	}
	if out.Participants, err = a.readParticipants(r.Context(), id); err != nil {
		serverError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// readParticipants returns what the study knows about its participants,
// ordered by label; never nil.
// Used by: getStudy, exportBIDS (bids.go).
func (a *api) readParticipants(ctx context.Context, studyID string) ([]participantJSON, error) {
	rows, err := a.db.Query(ctx, `SELECT label, age, sex FROM participants WHERE study_id = $1 ORDER BY label`, studyID)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (participantJSON, error) {
		var p participantJSON
		err := row.Scan(&p.Label, &p.Age, &p.Sex)
		return p, err
	})
	if out == nil {
		out = []participantJSON{}
	}
	return out, err
}

// setParticipant handles PUT /studies/{id}/participants/{label} with
// {"age": 24, "sex": "F"}: stores what the study knows about one participant,
// for participants.tsv. null and "" clear a value. Answers the stored values.
func (a *api) setParticipant(w http.ResponseWriter, r *http.Request) {
	id, label := r.PathValue("id"), r.PathValue("label")
	var body participantJSON
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, `the body must be {"age": number or null, "sex": "M", "F", "O" or ""}`)
		return
	}
	switch {
	case !bidsLabel.MatchString(label):
		writeError(w, http.StatusBadRequest, "the participant must be 1 to 32 letters or digits")
		return
	case body.Age != nil && (*body.Age < 0 || *body.Age > maxAge):
		writeError(w, http.StatusBadRequest, "the age must be 0 to 120 years")
		return
	case body.Sex != "" && body.Sex != "M" && body.Sex != "F" && body.Sex != "O":
		writeError(w, http.StatusBadRequest, `sex must be "M", "F", "O" or ""`)
		return
	}
	body.Label = label
	_, err := a.db.Exec(r.Context(), `
		INSERT INTO participants (study_id, label, age, sex) VALUES ($1, $2, $3, $4)
		ON CONFLICT (study_id, label) DO UPDATE SET age = EXCLUDED.age, sex = EXCLUDED.sex`,
		id, label, body.Age, body.Sex)
	var pgErr *pgconn.PgError
	switch {
	case errors.As(err, &pgErr) && pgErr.Code == "23503": // foreign_key_violation
		writeError(w, http.StatusNotFound, "no such study")
	case err != nil:
		serverError(w, r, err)
	default:
		writeJSON(w, http.StatusOK, body)
	}
}

// deleteStudy handles DELETE /studies/{id}: removes the study. Its sessions
// are kept, untagged.
func (a *api) deleteStudy(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var deleted int64
	err := pgx.BeginFunc(r.Context(), a.db, func(tx pgx.Tx) error {
		if _, err := tx.Exec(r.Context(),
			`UPDATE sessions SET study_id = NULL, participant = '', task = '' WHERE study_id = $1`, id); err != nil {
			return err
		}
		tag, err := tx.Exec(r.Context(), `DELETE FROM studies WHERE id = $1`, id) // its participants go with it
		deleted = tag.RowsAffected()
		return err
	})
	switch {
	case err != nil:
		serverError(w, r, err)
	case deleted == 0:
		writeError(w, http.StatusNotFound, "no such study")
	default:
		writeJSON(w, http.StatusOK, map[string]string{"deleted": id})
	}
}

// setSessionStudy handles PUT /sessions/{id}/study with {"studyId",
// "participant", "task", "visit"} (visit optional): puts the session in a study with its tags, moves it
// to another, or with "studyId": "" takes it out. Answers the updated session.
func (a *api) setSessionStudy(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var body studyTagJSON
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, `the body must be {"studyId", "participant", "task"}`)
		return
	}
	var study *string
	if body.StudyID == "" {
		body.Participant, body.Task, body.Visit = "", "", ""
	} else {
		if !bidsLabel.MatchString(body.Participant) || !bidsLabel.MatchString(body.Task) ||
			(body.Visit != "" && !bidsLabel.MatchString(body.Visit)) {
			writeError(w, http.StatusBadRequest, "participant, task and visit must be 1 to 32 letters or digits (a BIDS rule: they become file names)")
			return
		}
		study = &body.StudyID
	}
	tag, err := a.db.Exec(r.Context(),
		`UPDATE sessions SET study_id = $2, participant = $3, task = $4, visit = $5 WHERE id = $1`,
		id, study, body.Participant, body.Task, body.Visit)
	var pgErr *pgconn.PgError
	switch {
	case errors.As(err, &pgErr) && pgErr.Code == "23503": // foreign_key_violation
		writeError(w, http.StatusNotFound, "no such study")
		return
	case err != nil:
		serverError(w, r, err)
		return
	case tag.RowsAffected() == 0:
		writeError(w, http.StatusNotFound, "no such session")
		return
	}
	a.getSession(w, r)
}
