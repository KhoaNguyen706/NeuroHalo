package main

import (
	"encoding/json"
	"testing"
	"time"

	neurov1 "github.com/neurohalo/neurohalo/gen/neuro/v1"
)

// markerBatch builds one 100 ms batch of silence for a one-sensor session,
// carrying the given markers.
func markerBatch(seq uint64, markers ...*neurov1.Marker) *neurov1.SampleBatch {
	return &neurov1.SampleBatch{
		Seq: seq, FirstSampleIndex: seq * 16, NumSamples: 16, Values: make([]float32, 16), Markers: markers,
	}
}

func TestMarkerBecomesEventAtItsSample(t *testing.T) {
	start := time.Unix(1000, 0)
	s := newSession("t", testRate, []string{"Cz"}, start, nil)
	// Sample 200 is 1.25 s into the recording; the 2-second buffer is not full
	// yet, so no window has been computed. The marker must come out anyway.
	events := s.add(markerBatch(12, &neurov1.Marker{SampleIndex: 200, Label: "T1", DurationSeconds: 4.1})).events
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	e := events[0]
	if want := start.Add(1250 * time.Millisecond).UnixNano(); e.Type != "marker" || e.Channel != "" || e.TimeUnixNanos != want {
		t.Errorf("got type %q channel %q time %d, want marker, no channel, time %d", e.Type, e.Channel, e.TimeUnixNanos, want)
	}
	var details struct {
		Label    string
		Sample   uint64
		Duration float64
	}
	if err := json.Unmarshal([]byte(e.DetailsJson), &details); err != nil {
		t.Fatal(err)
	}
	if details.Label != "T1" || details.Sample != 200 || details.Duration != 4.1 {
		t.Errorf("details %+v, want label T1, sample 200, duration 4.1", details)
	}
}

func TestMarkersOnOneSampleAreJoined(t *testing.T) {
	s := newSession("t", testRate, []string{"Cz"}, time.Unix(0, 0), nil)
	events := s.add(markerBatch(0,
		&neurov1.Marker{SampleIndex: 3, Label: "cue"},
		&neurov1.Marker{SampleIndex: 9, Label: "beep"},
		&neurov1.Marker{SampleIndex: 3, Label: "left"},
	)).events
	if len(events) != 2 {
		t.Fatalf("got %d events, want 2", len(events))
	}
	var details struct{ Label string }
	_ = json.Unmarshal([]byte(events[0].DetailsJson), &details)
	if details.Label != "cue + left" {
		t.Errorf("label %q, want %q", details.Label, "cue + left")
	}
}

func TestRepeatedBatchGivesNoSecondMarker(t *testing.T) {
	s := newSession("t", testRate, []string{"Cz"}, time.Unix(0, 0), nil)
	b := markerBatch(0, &neurov1.Marker{SampleIndex: 3, Label: "cue"})
	if n := len(s.add(b).events); n != 1 {
		t.Fatalf("first time: %d events, want 1", n)
	}
	if res := s.add(b); !res.duplicate || len(res.events) != 0 {
		t.Errorf("second time: duplicate %v with %d events, want a duplicate with none", res.duplicate, len(res.events))
	}
}
