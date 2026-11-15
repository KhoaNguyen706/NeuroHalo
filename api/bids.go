package main

import (
	"archive/zip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/minio/minio-go/v7"

	"github.com/neurohalo/neurohalo/internal/eeg"
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
//	    sub-01_task-rest_channels.tsv  every sensor, as named in the EDF file, and
//	                                   whether the live check found it bad
//	    sub-01_task-rest_events.tsv    the markers, when there are any
//
// Only sessions whose recording is ready are exported; the README names the
// others. Several sessions of one participant and task get _run-1, _run-2...
// in the order they started.

// bidsVersion is the version of the BIDS specification the export follows.
const bidsVersion = "1.10.0"

// bidsRun is one session as it appears in the dataset.
// Built by: exportBIDS. Numbered by: numberRuns. Used by: bidsMetadata and its
// helpers (prefix, eegSidecar, sensorStatus, channelsTSV, eventsTSV), and exportBIDS, which
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
	Flags       []sensorFlag // the live check's events, in recording order
}

// sensorFlag is one channel_bad or channel_recovered event of a run.
// Read by: exportBIDS. Used by: sensorStatus.
type sensorFlag struct {
	Channel string
	Seconds float64 // when the live check flagged it, on the recording's clock
	Bad     bool    // channel_bad; false for channel_recovered
	Reason  string  // flat or noisy, for channel_bad
}

// badShare is how much of a recording a sensor must have been flagged for to
// get status "bad" in channels.tsv: more than half. A sensor flagged for less
// is still described, so a stricter lab can apply its own rule.
const badShare = 0.5

// sensorState is what channels.tsv says about one sensor.
// Built by: sensorStatus. Used by: channelsTSV.
type sensorState struct {
	Bad         bool
	Description string // "n/a" when the live check never flagged it
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

// sensorStatus turns the run's flags into a state per sensor: the stretches
// from each channel_bad to the next channel_recovered (or to the end of the
// recording) are added up, and a sensor flagged for more than badShare of
// the recording is bad. A flag for a sensor the run does not have is ignored.
// Used by: channelsTSV.
func (r bidsRun) sensorStatus() map[string]sensorState {
	type tally struct {
		since   float64 // when the current stretch began; -1 while not flagged
		total   float64 // seconds flagged so far
		first   float64 // when the first stretch began
		stretch int     // stretches so far
		reasons []string
	}
	t := map[string]*tally{}
	for _, c := range r.Channels {
		t[c] = &tally{since: -1}
	}
	end := func(a *tally, at float64) {
		if a.since >= 0 {
			a.total += math.Max(0, math.Min(at, r.Seconds)-a.since)
			a.since = -1
		}
	}
	for _, f := range r.Flags {
		a := t[f.Channel]
		switch {
		case a == nil:
		case f.Bad:
			if a.since < 0 {
				a.since = math.Min(f.Seconds, r.Seconds)
				if a.stretch == 0 {
					a.first = a.since
				}
				a.stretch++
			}
			if !slices.Contains(a.reasons, f.Reason) {
				a.reasons = append(a.reasons, f.Reason)
			}
		default:
			end(a, f.Seconds)
		}
	}
	out := map[string]sensorState{}
	for c, a := range t {
		end(a, r.Seconds)
		if a.stretch == 0 || r.Seconds <= 0 {
			out[c] = sensorState{Description: "n/a"}
			continue
		}
		share := a.total / r.Seconds
		from := seconds(math.Round(a.first*10) / 10)
		where := "from " + from + " s"
		if a.stretch > 1 {
			where = fmt.Sprintf("in %d stretches, the first from %s s", a.stretch, from)
		}
		out[c] = sensorState{
			Bad: share > badShare,
			Description: fmt.Sprintf("flagged %s by the live check for %.0f of %.0f s (%.0f%%), %s",
				strings.Join(a.reasons, " and "), a.total, r.Seconds, share*100, where),
		}
	}
	return out
}

// channelsTSV is the run's _channels.tsv: every sensor under the name the EDF
// header gives it, all of type EEG in microvolts, with its status from the
// live check (sensorStatus). MNE-BIDS and EEGLAB read status "bad" as a
// sensor to leave out.
// Used by: bidsMetadata.
func (r bidsRun) channelsTSV() []byte {
	status := r.sensorStatus()
	var b strings.Builder
	b.WriteString("name\ttype\tunits\tstatus\tstatus_description\n")
	for _, c := range r.Channels {
		st, word := status[c], "good"
		if st.Bad {
			word = "bad"
		}
		b.WriteString(tsvField(recording.EDFLabel(c)) + "\tEEG\tµV\t" + word + "\t" + tsvField(st.Description) + "\n")
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
		"NeuroHalo and are given as n/a in each _eeg.json.\n\n" +
		"While recording, NeuroHalo checks every sensor twice a second, over the last 2 seconds: flat " +
		"(less than 0.5 uV of variation) or noisy (more than 500 uV peak to peak). In each channels.tsv, " +
		"a sensor flagged for more than half of the recording has status \"bad\", and status_description " +
		"says for how long and from when any sensor was flagged, so a stricter rule can be applied. " +
		"The times are when the live check flagged it, up to about 3 seconds after the problem began.\n")
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

	// The live check's flags. Each carries its analysis window, which ends at
	// sample window x hop: an exact time on the recording's clock.
	rows, err = a.db.Query(ctx, `
		SELECT session_id, channel, type = 'channel_bad', (details->>'window')::bigint, COALESCE(details->>'reason', '')
		FROM events WHERE type IN ('channel_bad', 'channel_recovered') AND session_id = ANY($1)
		ORDER BY session_id, (details->>'window')::bigint, id`, ids)
	if err != nil {
		serverError(w, r, err)
		return
	}
	for rows.Next() {
		var sessionID string
		var f sensorFlag
		var window int64
		if err := rows.Scan(&sessionID, &f.Channel, &f.Bad, &window, &f.Reason); err != nil {
			rows.Close()
			serverError(w, r, err)
			return
		}
		run := &runs[byID[sessionID]]
		f.Seconds = float64(window*int64(eeg.HopSamples(run.RateHz))) / run.RateHz
		run.Flags = append(run.Flags, f)
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
