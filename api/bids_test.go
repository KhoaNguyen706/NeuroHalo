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
	age := 31
	files := map[string]string{}
	for _, f := range bidsMetadata("Motor pilot", time.Unix(0, 0), runs, []string{"session s-x: its recording is not finished"},
		[]participantJSON{{Label: "02", Age: &age, Sex: "F"}}) {
		files[f.Path] = string(f.Body)
	}
	checks := map[string]string{
		"participants.tsv": "participant_id\tage\tsex\nsub-01\tn/a\tn/a\nsub-02\t31\tF\n",
		"sub-01/eeg/sub-01_task-fists_channels.tsv": "name\ttype\tunits\tstatus\tstatus_description\n" +
			"C3\tEEG\tµV\tgood\tn/a\nCz\tEEG\tµV\tgood\tn/a\na very long sens\tEEG\tµV\tgood\tn/a\n",
		"sub-01/eeg/sub-01_task-fists_events.tsv": "onset\tduration\ttrial_type\tsample\n4.2\t4.2\tT1\t672\n8.4\t0\twith tab\t1344\n",
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

// TestSensorStatus checks how the live check's flags become channels.tsv
// status: stretches are added up to the next recovery or the end, more than
// half the recording is bad, and every flagged sensor is described.
func TestSensorStatus(t *testing.T) {
	run := bidsRun{
		Seconds:  100,
		Channels: []string{"Cz", "O1", "Fp1", "C3"},
		Flags: []sensorFlag{
			{Channel: "Fp1", Seconds: 10, Bad: true, Reason: "noisy"},
			{Channel: "Cz", Seconds: 20.04, Bad: true, Reason: "flat"},
			{Channel: "Fp1", Seconds: 20},
			{Channel: "Fp1", Seconds: 50, Bad: true, Reason: "noisy"},
			{Channel: "Fp1", Seconds: 60, Bad: true, Reason: "flat"}, // still bad, new reason
			{Channel: "Fp1", Seconds: 70},
			{Channel: "C3", Seconds: 70, Bad: true, Reason: "noisy"}, // to the end: exactly 30%
			{Channel: "X9", Seconds: 5, Bad: true, Reason: "flat"},   // not a sensor of this run
		},
	}
	got := run.sensorStatus()
	want := map[string]sensorState{
		"Cz":  {true, "flagged flat by the live check for 80 of 100 s (80%), from 20 s"},
		"O1":  {false, "n/a"},
		"Fp1": {false, "flagged noisy and flat by the live check for 30 of 100 s (30%), in 2 stretches, the first from 10 s"},
		"C3":  {false, "flagged noisy by the live check for 30 of 100 s (30%), from 70 s"},
	}
	if len(got) != len(want) {
		t.Errorf("%d sensors, want %d: %v", len(got), len(want), got)
	}
	for c, w := range want {
		if got[c] != w {
			t.Errorf("%s: %+v, want %+v", c, got[c], w)
		}
	}
}

// TestVisits checks the ses- level: a participant's visits get their own
// folders and runs are numbered within each visit, and a session without a
// visit is left out when the same participant has visits.
func TestVisits(t *testing.T) {
	t0 := time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)
	runs := []bidsRun{
		{SessionID: "a", Participant: "01", Visit: "pre", Task: "rest", StartedAt: t0},
		{SessionID: "b", Participant: "01", Visit: "post", Task: "rest", StartedAt: t0.Add(time.Hour)},
		{SessionID: "c", Participant: "01", Visit: "post", Task: "rest", StartedAt: t0.Add(2 * time.Hour)},
		{SessionID: "d", Participant: "01", Task: "rest", StartedAt: t0},
		{SessionID: "e", Participant: "02", Task: "rest", StartedAt: t0},
	}
	kept, skipped := dropUnvisited(runs)
	if len(skipped) != 1 || !strings.Contains(skipped[0], "session d ") {
		t.Fatalf("skipped %v, want only session d", skipped)
	}
	numberRuns(kept)
	want := []string{
		"sub-01/ses-post/eeg/sub-01_ses-post_task-rest_run-1", // b
		"sub-01/ses-post/eeg/sub-01_ses-post_task-rest_run-2", // c
		"sub-01/ses-pre/eeg/sub-01_ses-pre_task-rest",         // a
		"sub-02/eeg/sub-02_task-rest",                         // e
	}
	if len(kept) != len(want) {
		t.Fatalf("%d runs kept, want %d", len(kept), len(want))
	}
	for i, r := range kept {
		if r.prefix() != want[i] {
			t.Errorf("run %d (%s): %s, want %s", i, r.SessionID, r.prefix(), want[i])
		}
	}
}
