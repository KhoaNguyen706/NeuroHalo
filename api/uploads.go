package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/jackc/pgx/v5"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

const (
	maxUploadBytes = 50 << 20 // a two-minute run in the dataset is 2.4 MB
	uploadPrefix   = "uploads/"

	replayFromMark        = " · from "   // added to the label of a "replay from here" session
	replayPartMark        = " · part "   // added to the label of a "replay from A to B" session
	minReplaySeconds      = 2            // the processor needs 2 seconds for one frame
	maxReplayStartSeconds = 24 * 60 * 60 // "replay from" beyond a day is a mistake
)

// sample is one built-in recording that can be played with one click.
// Listed by: api.listSamples. Played by: api.playSample.
type sample struct {
	Name        string `json:"name"`
	Title       string `json:"title"`
	Description string `json:"description"`
	file        string // PhysioNet file name; the worker downloads it
	flatten     string // CHANNEL@SECONDS, or empty
}

// The three demo samples, all from subject 1's baseline runs of the PhysioNet
// EEG Motor Movement/Imagery dataset (ODC-By 1.0).
var samples = []sample{
	{Name: "eyes-open", Title: "Eyes open", file: "S001R01.edf",
		Description: "One minute at rest with eyes open. Back-of-head alpha stays low."},
	{Name: "eyes-closed", Title: "Eyes closed", file: "S001R02.edf",
		Description: "One minute at rest with eyes closed. Back-of-head alpha is about 3.7 times stronger."},
	{Name: "eyes-closed-flat-cz", Title: "Eyes closed, Cz fails", file: "S001R02.edf", flatten: "Cz@20",
		Description: "The same run with sensor Cz going flat at 20 s, to show bad-sensor detection."},
}

// objectStore connects to the S3-compatible store that keeps uploaded files.
// Created by: newObjectStore. Used by: api.upload.
type objectStore struct {
	client *minio.Client
	bucket string
}

// newObjectStore builds the S3 client. It does not contact the store.
func newObjectStore(endpoint, accessKey, secretKey, bucket string, secure bool) (*objectStore, error) {
	cl, err := minio.New(endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(accessKey, secretKey, ""),
		Secure: secure,
	})
	if err != nil {
		return nil, err
	}
	return &objectStore{client: cl, bucket: bucket}, nil
}

// ensureBucket creates the bucket if needed, retrying while the store starts up.
func (o *objectStore) ensureBucket(ctx context.Context) {
	backoff := time.Second
	for ctx.Err() == nil {
		exists, err := o.client.BucketExists(ctx, o.bucket)
		if err == nil && !exists {
			err = o.client.MakeBucket(ctx, o.bucket, minio.MakeBucketOptions{})
		}
		if err == nil {
			log.Printf("object store ready, bucket %q", o.bucket)
			return
		}
		log.Printf("object store not ready, retrying in %v: %v", backoff, err)
		select {
		case <-ctx.Done():
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 10*time.Second)
	}
}

// newID returns prefix + 12 random hex characters, e.g. "u-3f9a1c0b7d2e".
func newID(prefix string) string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return prefix + hex.EncodeToString(b)
}

// cleanLabel turns a user's file name into a safe display label: base name
// only, printable characters only, at most 100 characters. It is shown on the
// page but never used to name the stored file.
func cleanLabel(name string) string {
	name = filepath.Base(strings.ReplaceAll(name, "\\", "/"))
	name = strings.Map(func(r rune) rune {
		if unicode.IsPrint(r) {
			return r
		}
		return -1
	}, name)
	if len([]rune(name)) > 100 {
		name = string([]rune(name)[:100])
	}
	if name == "" || name == "." || name == "/" {
		return "upload.edf"
	}
	return name
}

// replaySpeed reads the "speed" form or query value: "1x" (default) or "fast".
func replaySpeed(r *http.Request) (string, bool) {
	switch s := r.FormValue("speed"); s {
	case "", "1x":
		return "1x", true
	case "fast":
		return "fast", true
	}
	return "", false
}

// queueReplay inserts the job the replay worker picks up. startSeconds and
// endSeconds are the part of the recording to play; 0 and 0 play all of it,
// and an endSeconds of 0 plays to the end.
func (a *api) queueReplay(ctx context.Context, sessionID, kind, label, objectKey, sampleFile, speed, flatten string, startSeconds, endSeconds float64) error {
	_, err := a.db.Exec(ctx, `
		INSERT INTO replays (session_id, kind, label, object_key, sample, speed, flatten, start_seconds, end_seconds)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		sessionID, kind, label, objectKey, sampleFile, speed, flatten, startSeconds, endSeconds)
	return err
}

// replaySeconds reads an optional query value holding seconds into the
// recording. A missing value is 0; ok is false for anything but a number
// from 0 to maxReplayStartSeconds.
func replaySeconds(r *http.Request, name string) (float64, bool) {
	text := r.FormValue(name)
	if text == "" {
		return 0, true
	}
	v, err := strconv.ParseFloat(text, 64)
	if err != nil || math.IsNaN(v) || v < 0 || v > maxReplayStartSeconds {
		return 0, false
	}
	return v, true
}

// clockText formats seconds as m:ss for a session label.
func clockText(seconds float64) string {
	return fmt.Sprintf("%d:%02d", int(seconds)/60, int(seconds)%60)
}

// replayFrom handles POST /sessions/{id}/replay?from=SECONDS&to=SECONDS&speed=1x|fast:
// queues a new replay of the recording behind session {id}, from `from`
// seconds into it up to `to` seconds (to the end when `to` is left out),
// and returns the new session's ID. Only sessions
// that came from a sample or an upload can be replayed; one streamed by hand
// has no stored recording.
func (a *api) replayFrom(w http.ResponseWriter, r *http.Request) {
	speed, ok := replaySpeed(r)
	if !ok {
		writeError(w, http.StatusBadRequest, `speed must be "1x" or "fast"`)
		return
	}
	from, okFrom := replaySeconds(r, "from")
	to, okTo := replaySeconds(r, "to")
	switch {
	case !okFrom || !okTo:
		writeError(w, http.StatusBadRequest, "from and to must be numbers of seconds, 0 or more")
		return
	case to > 0 && to-from < minReplaySeconds:
		writeError(w, http.StatusBadRequest, "to must be at least 2 seconds after from")
		return
	}
	var kind, label, objectKey, sampleFile, flatten string
	err := a.db.QueryRow(r.Context(), `
		SELECT kind, label, object_key, sample, flatten FROM replays WHERE session_id = $1`,
		r.PathValue("id")).Scan(&kind, &label, &objectKey, &sampleFile, &flatten)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		writeError(w, http.StatusNotFound, "this session has no stored recording to replay")
		return
	case err != nil:
		serverError(w, r, err)
		return
	}
	// Name the new session after the part it plays: "Eyes closed · from 0:30"
	// or "Eyes closed · 0:20–0:40". A replay of a replay keeps one such ending, not two.
	for _, mark := range []string{replayFromMark, replayPartMark} {
		if i := strings.Index(label, mark); i >= 0 {
			label = label[:i]
		}
	}
	switch {
	case to > 0:
		label += replayPartMark + clockText(from) + "–" + clockText(to)
	case from > 0:
		label += replayFromMark + clockText(from)
	}
	id := newID("s-")
	if err := a.queueReplay(r.Context(), id, kind, label, objectKey, sampleFile, speed, flatten, from, to); err != nil {
		serverError(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"sessionId": id})
}

// listSamples handles GET /samples.
func (a *api) listSamples(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, samples)
}

// playSample handles POST /samples/{name}/play?speed=1x|fast: queues a replay
// of a built-in recording and returns the new session's ID.
func (a *api) playSample(w http.ResponseWriter, r *http.Request) {
	speed, ok := replaySpeed(r)
	if !ok {
		writeError(w, http.StatusBadRequest, `speed must be "1x" or "fast"`)
		return
	}
	for _, s := range samples {
		if s.Name != r.PathValue("name") {
			continue
		}
		id := newID("s-")
		if err := a.queueReplay(r.Context(), id, "sample", s.Title, "", s.file, speed, s.flatten, 0, 0); err != nil {
			serverError(w, r, err)
			return
		}
		writeJSON(w, http.StatusAccepted, map[string]string{"sessionId": id})
		return
	}
	writeError(w, http.StatusNotFound, "no such sample")
}

// upload handles POST /uploads (multipart, field "file", optional "speed").
// It accepts one EDF file of at most 50 MB, checks its header, stores it under
// a generated name, and queues a replay. The answer holds the new session's ID
// and what the header says.
func (a *api) upload(w http.ResponseWriter, r *http.Request) {
	if a.objects == nil {
		writeError(w, http.StatusServiceUnavailable, "uploads are not configured")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBytes+1<<20) // room for the form around the file
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeError(w, http.StatusRequestEntityTooLarge, "file is larger than 50 MB")
			return
		}
		writeError(w, http.StatusBadRequest, "want a multipart form with a field named file")
		return
	}
	defer r.MultipartForm.RemoveAll()
	speed, ok := replaySpeed(r)
	if !ok {
		writeError(w, http.StatusBadRequest, `speed must be "1x" or "fast"`)
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		writeError(w, http.StatusBadRequest, "want a multipart form with a field named file")
		return
	}
	defer file.Close()
	switch {
	case !strings.EqualFold(filepath.Ext(header.Filename), ".edf"):
		writeError(w, http.StatusBadRequest, "only .edf files are accepted")
		return
	case header.Size > maxUploadBytes:
		writeError(w, http.StatusRequestEntityTooLarge, "file is larger than 50 MB")
		return
	}
	info, err := parseEDFHeader(file, header.Size)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "not a readable EDF file: "+err.Error())
		return
	}

	id := newID("u-")
	key := uploadPrefix + id + ".edf"
	if _, err := a.objects.client.PutObject(r.Context(), a.objects.bucket, key, file, header.Size,
		minio.PutObjectOptions{ContentType: "application/octet-stream"}); err != nil {
		log.Printf("store upload %s: %v", key, err)
		writeError(w, http.StatusServiceUnavailable, "file storage is unreachable, try again")
		return
	}
	if err := a.queueReplay(r.Context(), id, "upload", cleanLabel(header.Filename), key, "", speed, "", 0, 0); err != nil {
		serverError(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"sessionId": id, "edf": info})
}

// replayJSON is a replay job's state, for the page to show while a session starts.
// Used by: api.getReplay.
type replayJSON struct {
	SessionID string `json:"sessionId"`
	Kind      string `json:"kind"`
	Label     string `json:"label"`
	Speed     string `json:"speed"`
	// Where in the recording the replay started; above 0 for "replay from here".
	StartSeconds float64 `json:"startSeconds"`
	// Where it stops; 0 when it plays to the end.
	EndSeconds float64   `json:"endSeconds"`
	Status     string    `json:"status"` // queued | running | done | failed
	Error      string    `json:"error"`
	CreatedAt  time.Time `json:"createdAt"`
}

// getReplay handles GET /replays/{id}: the state of the replay behind a session.
func (a *api) getReplay(w http.ResponseWriter, r *http.Request) {
	var out replayJSON
	err := a.db.QueryRow(r.Context(), `
		SELECT session_id, kind, label, speed, start_seconds, end_seconds, status, error, created_at FROM replays WHERE session_id = $1`,
		r.PathValue("id")).Scan(&out.SessionID, &out.Kind, &out.Label, &out.Speed, &out.StartSeconds, &out.EndSeconds, &out.Status, &out.Error, &out.CreatedAt)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		writeError(w, http.StatusNotFound, "no such replay")
	case err != nil:
		serverError(w, r, err)
	default:
		writeJSON(w, http.StatusOK, out)
	}
}
