package main

import (
	"strings"
	"testing"
	"time"
)

// TestNumberRuns checks that runs get numbers only when a participant has
// more than one session of a task, in the order the sessions started.
func TestNumberRuns(t *testing.T) {
	t0 := time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC)
	runs := []bidsRun{
		{SessionID: "c", Participant: "01", Task: "fists", StartedAt: t0.Add(2 * time.Hour)},
		{SessionID: "b", Participant: "01", Task: "rest", StartedAt: t0},
		{SessionID: "a", Participant: "01", Task: "fists", StartedAt: t0},
		{SessionID: "d", Participant: "02", Task: "fists", StartedAt: t0},
	}
	numberRuns(runs)
	want := []string{
		"sub-01/eeg/sub-01_task-fists_run-1", // a
		"sub-01/eeg/sub-01_task-fists_run-2", // c, later
		"sub-01/eeg/sub-01_task-rest",
		"sub-02/eeg/sub-02_task-fists",
	}
	for i, r := range runs {
		if r.prefix() != want[i] {
			t.Errorf("run %d (%s): %s, want %s", i, r.SessionID, r.prefix(), want[i])
		}
	}
}

// TestBIDSMetadata checks the side files of a small dataset: one line per
// participant, sensors under their EDF names, markers in seconds, and no
// events file for a run without markers.
func TestBIDSMetadata(t *testing.T) {
	runs := []bidsRun{
		{Participant: "01", Task: "fists", RateHz: 160, Seconds: 125, Channels: []string{"C3", "Cz", "a very long sensor name"},
			Markers: []bidsMarker{{Sample: 672, Duration: 4.2, Label: "T1"}, {Sample: 1344, Label: "with\ttab"}}},
		{Participant: "01", Task: "rest", RateHz: 160, Seconds: 60, Channels: []string{"C3"}},
		{Participant: "02", Task: "rest", RateHz: 160, Seconds: 60, Channels: []string{"C3"}},
	}
	numberRuns(runs)
	files := map[string]string{}
	for _, f := range bidsMetadata("Motor pilot", time.Unix(0, 0), runs, []string{"session s-x: its recording is not finished"}) {
		files[f.Path] = string(f.Body)
	}
	checks := map[string]string{
		"participants.tsv":                          "participant_id\nsub-01\nsub-02\n",
		"sub-01/eeg/sub-01_task-fists_channels.tsv": "name\ttype\tunits\nC3\tEEG\tµV\nCz\tEEG\tµV\na very long sens\tEEG\tµV\n",
		"sub-01/eeg/sub-01_task-fists_events.tsv":   "onset\tduration\ttrial_type\tsample\n4.2\t4.2\tT1\t672\n8.4\t0\twith tab\t1344\n",
	}
	for path, want := range checks {
		if got := files[path]; got != want {
			t.Errorf("%s:\n%q\nwant\n%q", path, got, want)
		}
	}
	if _, ok := files["sub-01/eeg/sub-01_task-rest_events.tsv"]; ok {
		t.Error("a run without markers got an events file")
	}
	if _, ok := files["task-fists_events.json"]; !ok {
		t.Error("the events columns of task fists are not described")
	}
	if _, ok := files["task-rest_events.json"]; ok {
		t.Error("task rest has no events but got an events.json")
	}
	for _, path := range []string{"dataset_description.json", "README", "sub-02/eeg/sub-02_task-rest_eeg.json"} {
		if files[path] == "" {
			t.Errorf("%s is missing", path)
		}
	}
	if !strings.Contains(files["README"], "s-x") || !strings.Contains(files["sub-01/eeg/sub-01_task-fists_eeg.json"], `"SamplingFrequency": 160`) {
		t.Errorf("README or _eeg.json is missing a value:\n%s\n%s", files["README"], files["sub-01/eeg/sub-01_task-fists_eeg.json"])
	}
}
