package main

import (
	"archive/zip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/minio/minio-go/v7"

	"github.com/neurohalo/neurohalo/internal/recording"
)

// A study downloads as a BIDS dataset: the folder layout and side files that
// EEG labs use to share data (Brain Imaging Data Structure, EEG extension).
// EEGLAB, MNE-BIDS, FieldTrip and OpenNeuro read it as is:
//
//	<study>/
//	  dataset_description.json   name and BIDS version
//	  README                     what was exported, and what was left out
//	  participants.tsv           one line per participant
//	  task-rest_events.json      what the columns of events.tsv mean
//	  sub-01/eeg/
//	    sub-01_task-rest_eeg.edf       the recorder's EDF+ file, unchanged
//	    sub-01_task-rest_eeg.json      sample rate, sensor count, duration
//	    sub-01_task-rest_channels.tsv  every sensor, as named in the EDF file
//	    sub-01_task-rest_events.tsv    the markers, when there are any
//
// Only sessions whose recording is ready are exported; the README names the
// others. Several sessions of one participant and task get _run-1, _run-2...
// in the order they started.

// bidsVersion is the version of the BIDS specification the export follows.
const bidsVersion = "1.10.0"

// bidsRun is one session as it appears in the dataset.
// Built by: exportBIDS. Numbered by: numberRuns. Used by: bidsMetadata and its
// helpers (prefix, eegSidecar, channelsTSV, eventsTSV), and exportBIDS, which
// copies the EDF file.
type bidsRun struct {
	SessionID   string
	Participant string
	Task        string
	Run         int // 0 when this is the participant's only session of the task
	RateHz      float64
	Channels    []string
	Seconds     float64 // the signal the EDF file holds
	StartedAt   time.Time
	ObjectKey   string // the EDF file in the object store
	Markers     []bidsMarker
}

// bidsMarker is one marker of a run, from the session's "marker" events.
// Read by: exportBIDS. Used by: eventsTSV.
type bidsMarker struct {
	Sample   int64 // position in the recording; sample 0 is the file's first
	Duration float64
	Label    string
}

// bidsFile is one small file of the dataset, held in memory.
// Built by: bidsMetadata. Used by: exportBIDS, which writes it into the zip.
type bidsFile struct {
	Path string // relative to the dataset's folder
	Body []byte
}

// numberRuns sorts runs by participant, task and start time, and numbers the
// sessions of a participant and task that has more than one.
// Used by: exportBIDS.
func numberRuns(runs []bidsRun) {
	sort.SliceStable(runs, func(i, j int) bool {
		a, b := runs[i], runs[j]
		if a.Participant != b.Participant {
			return a.Participant < b.Participant
		}
		if a.Task != b.Task {
			return a.Task < b.Task
		}
		return a.StartedAt.Before(b.StartedAt)
	})
	for i := 0; i < len(runs); {
		j := i
		for j < len(runs) && runs[j].Participant == runs[i].Participant && runs[j].Task == runs[i].Task {
			j++
		}
		for k := i; k < j; k++ {
			runs[k].Run = 0
			if j-i > 1 {
				runs[k].Run = k - i + 1
			}
		}
		i = j
	}
}

// prefix is the start of every file name of the run, with its folder:
// "sub-01/eeg/sub-01_task-rest_run-2".
func (r bidsRun) prefix() string {
	name := "sub-" + r.Participant + "_task-" + r.Task
	if r.Run > 0 {
		name += "_run-" + strconv.Itoa(r.Run)
	}
	return "sub-" + r.Participant + "/eeg/" + name
}

// eegSidecar is the run's _eeg.json. The fields BIDS requires but NeuroHalo
// cannot know (the reference sensor, mains frequency, filters in the
// amplifier) are "n/a", which BIDS allows.
// Used by: bidsMetadata.
func (r bidsRun) eegSidecar() []byte {
	body, _ := json.MarshalIndent(struct {
		TaskName           string
		SamplingFrequency  float64
		EEGChannelCount    int
		EEGReference       string
		PowerLineFrequency string
		SoftwareFilters    string
		RecordingDuration  float64
		RecordingType      string
	}{r.Task, r.RateHz, len(r.Channels), "n/a", "n/a", "n/a", r.Seconds, "continuous"}, "", "  ")
	return append(body, '\n')
}

// channelsTSV is the run's _channels.tsv: every sensor under the name the EDF
// header gives it, all of type EEG in microvolts.
// Used by: bidsMetadata.
func (r bidsRun) channelsTSV() []byte {
	var b strings.Builder
	b.WriteString("name\ttype\tunits\n")
	for _, c := range r.Channels {
		b.WriteString(tsvField(recording.EDFLabel(c)) + "\tEEG\tµV\n")
	}
	return []byte(b.String())
}

// eventsTSV is the run's _events.tsv: one line per marker, with its onset and
// duration in seconds, its label as the trial type, and its sample. Nil when
// the run has no markers; the file is optional then.
// Used by: bidsMetadata.
func (r bidsRun) eventsTSV() []byte {
	if len(r.Markers) == 0 {
		return nil
	}
	var b strings.Builder
	b.WriteString("onset\tduration\ttrial_type\tsample\n")
	for _, m := range r.Markers {
		fmt.Fprintf(&b, "%s\t%s\t%s\t%d\n", seconds(float64(m.Sample)/r.RateHz), seconds(m.Duration),
			tsvField(m.Label), m.Sample)
	}
	return []byte(b.String())
}

// seconds formats a time in seconds with up to 6 decimals and no trailing zeros.
// Used by: eventsTSV.
func seconds(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
}

// tsvField makes a value safe for one TSV cell: tabs and line breaks become
// spaces, and an empty value is "n/a", as BIDS writes a missing value.
// Used by: channelsTSV, eventsTSV.
func tsvField(s string) string {
	s = strings.Map(func(r rune) rune {
		if r == '\t' || r == '\n' || r == '\r' {
			return ' '
		}
		return r
	}, s)
	if strings.TrimSpace(s) == "" {
		return "n/a"
	}
	return s
}

// bidsMetadata builds every file of the dataset except the EDF files:
// dataset_description.json, README, participants.tsv and each run's side
// files. runs must already be numbered. skipped lists the sessions of the
// study that were left out, with the reason, for the README.
// Used by: exportBIDS.
func bidsMetadata(study string, exported time.Time, runs []bidsRun, skipped []string) []bidsFile {
	description, _ := json.MarshalIndent(map[string]any{
		"Name":        study,
		"BIDSVersion": bidsVersion,
		"DatasetType": "raw",
		"GeneratedBy": []map[string]string{{
			"Name":        "NeuroHalo",
			"Description": "Live EEG pipeline; each recording was saved by its recorder as it streamed",
		}},
	}, "", "  ")

	var participants []string
	for _, r := range runs {
		if len(participants) == 0 || participants[len(participants)-1] != r.Participant {
			participants = append(participants, r.Participant) // runs are sorted by participant
		}
	}
	var tsv strings.Builder
	tsv.WriteString("participant_id\n")
	for _, p := range participants {
		tsv.WriteString("sub-" + p + "\n")
	}

	var readme strings.Builder
	fmt.Fprintf(&readme, "%s\n\nExported from NeuroHalo on %s: %d recordings of %d participants.\n\n",
		study, exported.UTC().Format("2006-01-02 15:04 UTC"), len(runs), len(participants))
	readme.WriteString("Each recording is the raw signal as it streamed through NeuroHalo, saved as EDF+ " +
		"with 16 bits per sample, scaled to each sensor's own range. Markers are listed in each " +
		"recording's events.tsv and are also inside the EDF file as annotations. A stretch where no " +
		"signal arrived is filled with zeros and annotated \"NeuroHalo: no data\" in the EDF file.\n\n" +
		"The reference sensor, the mains frequency and the amplifier's filters were not known to " +
		"NeuroHalo and are given as n/a in each _eeg.json.\n")
	if len(skipped) > 0 {
		readme.WriteString("\nLeft out:\n")
		for _, s := range skipped {
			readme.WriteString("- " + s + "\n")
		}
	}

	files := []bidsFile{
		{"dataset_description.json", append(description, '\n')},
		{"README", []byte(readme.String())},
		{"participants.tsv", []byte(tsv.String())},
	}
	described := map[string]bool{} // tasks whose events.tsv columns are described
	for _, r := range runs {
		p := r.prefix()
		files = append(files, bidsFile{p + "_eeg.json", r.eegSidecar()}, bidsFile{p + "_channels.tsv", r.channelsTSV()})
		if events := r.eventsTSV(); events != nil {
			files = append(files, bidsFile{p + "_events.tsv", events})
			if !described[r.Task] {
				described[r.Task] = true
				files = append(files, bidsFile{"task-" + r.Task + "_events.json", eventsSidecar})
			}
		}
	}
	return files
}

// eventsSidecar describes the columns of events.tsv. One copy at the top of
// the dataset per task applies to every run of that task (BIDS inheritance).
// Used by: bidsMetadata.
var eventsSidecar = []byte(`{
  "trial_type": {
    "Description": "The marker's label, as the recording's annotations or the experiment software's marker stream gave it"
  },
  "sample": {
    "Description": "The marker's sample in the recording; the first sample is 0"
  }
}
`)

// safeName keeps letters, digits, '.', '-' and '_' of s and turns anything
// else into '_', for a file or folder name. Empty becomes "study".
// Used by: downloadName (recordings.go), exportBIDS.
func safeName(s string) string {
	s = strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
			return r
		}
		return '_'
	}, strings.TrimSpace(s))
	if strings.Trim(s, "._") == "" {
		return "study"
	}
	return s
}

// exportBIDS handles GET /studies/{id}/bids.zip: the study as a BIDS dataset
// in one zip, streamed as it is written. 409 when no session of the study has
// a finished recording yet.
func (a *api) exportBIDS(w http.ResponseWriter, r *http.Request) {
	if a.objects == nil {
		writeError(w, http.StatusServiceUnavailable, "the object store is not configured")
		return
	}
	ctx := r.Context()
	id := r.PathValue("id")
	var study string
	err := a.db.QueryRow(ctx, `SELECT name FROM studies WHERE id = $1`, id).Scan(&study)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		writeError(w, http.StatusNotFound, "no such study")
		return
	case err != nil:
		serverError(w, r, err)
		return
	}

	var runs []bidsRun
	var skipped []string
	byID := map[string]int{}
	rows, err := a.db.Query(ctx, `
		SELECT s.id, s.participant, s.task, s.sample_rate_hz, s.channel_names, s.started_at,
			COALESCE(rec.status, ''), COALESCE(rec.object_key, ''), COALESCE(rec.seconds, 0)
		FROM sessions s LEFT JOIN recordings rec ON rec.session_id = s.id
		WHERE s.study_id = $1`, id)
	if err != nil {
		serverError(w, r, err)
		return
	}
	for rows.Next() {
		var run bidsRun
		var status string
		if err := rows.Scan(&run.SessionID, &run.Participant, &run.Task, &run.RateHz, &run.Channels,
			&run.StartedAt, &status, &run.ObjectKey, &run.Seconds); err != nil {
			rows.Close()
			serverError(w, r, err)
			return
		}
		if status != "ready" {
			reason := "its recording is not finished"
			if status == "" {
				reason = "nothing was recorded"
			} else if status == "failed" {
				reason = "its recording could not be written"
			}
			skipped = append(skipped, fmt.Sprintf("session %s (participant %s, task %s): %s",
				run.SessionID, run.Participant, run.Task, reason))
			continue
		}
		runs = append(runs, run)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		serverError(w, r, err)
		return
	}
	if len(runs) == 0 {
		writeError(w, http.StatusConflict, "no session in this study has a finished recording yet")
		return
	}
	numberRuns(runs)
	ids := make([]string, len(runs))
	for i, run := range runs {
		ids[i] = run.SessionID
		byID[run.SessionID] = i
	}

	rows, err = a.db.Query(ctx, `
		SELECT session_id, (details->>'sample')::bigint, COALESCE((details->>'duration')::float8, 0),
			COALESCE(details->>'label', '')
		FROM events WHERE type = 'marker' AND session_id = ANY($1)
		ORDER BY session_id, (details->>'sample')::bigint`, ids)
	if err != nil {
		serverError(w, r, err)
		return
	}
	for rows.Next() {
		var sessionID string
		var m bidsMarker
		if err := rows.Scan(&sessionID, &m.Sample, &m.Duration, &m.Label); err != nil {
			rows.Close()
			serverError(w, r, err)
			return
		}
		run := &runs[byID[sessionID]]
		run.Markers = append(run.Markers, m)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		serverError(w, r, err)
		return
	}

	folder := safeName(study)
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="`+folder+`-bids.zip"`)
	zw := zip.NewWriter(w)
	now := time.Now()
	add := func(path string) io.Writer {
		f, err := zw.CreateHeader(&zip.FileHeader{Name: folder + "/" + path, Method: zip.Deflate, Modified: now})
		if err != nil {
			abortDownload(id, err)
		}
		return f
	}
	for _, f := range bidsMetadata(study, now, runs, skipped) {
		if _, err := add(f.Path).Write(f.Body); err != nil {
			abortDownload(id, err)
		}
	}
	for _, run := range runs {
		obj, err := a.objects.client.GetObject(ctx, a.objects.bucket, run.ObjectKey, minio.GetObjectOptions{})
		if err == nil {
			_, err = io.Copy(add(run.prefix()+"_eeg.edf"), obj)
			obj.Close()
		}
		if err != nil {
			abortDownload(id, fmt.Errorf("%s: %w", run.ObjectKey, err))
		}
	}
	if err := zw.Close(); err != nil {
		abortDownload(id, err)
	}
}

// abortDownload logs why a study's zip could not be finished and cuts the
// connection. The status line is already sent by then, so this is the only way
// to make the browser report a failed download instead of saving a broken zip.
// Used by: exportBIDS.
func abortDownload(studyID string, err error) {
	log.Printf("study %s: BIDS export stopped: %v", studyID, err)
	panic(http.ErrAbortHandler)
}
