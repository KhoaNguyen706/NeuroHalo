package main

import (
	"strings"
	"testing"

	neurov1 "github.com/neurohalo/neurohalo/gen/neuro/v1"
)

func TestValidateMarkers(t *testing.T) {
	header := &neurov1.SessionHeader{SessionId: "s", SampleRateHz: 160, ChannelNames: []string{"Cz"}}
	// batch covers samples 160 to 175 and carries the given markers.
	batch := func(markers ...*neurov1.Marker) *neurov1.SampleBatch {
		return &neurov1.SampleBatch{
			SessionId: "s", Seq: 10, FirstSampleIndex: 160, NumSamples: 16, Values: make([]float32, 16), Markers: markers,
		}
	}
	many := make([]*neurov1.Marker, maxMarkersPerBatch+1)
	for i := range many {
		many[i] = &neurov1.Marker{SampleIndex: 160, Label: "x"}
	}
	cases := []struct {
		name    string
		markers []*neurov1.Marker
		ok      bool
	}{
		{"no markers", nil, true},
		{"first and last sample", []*neurov1.Marker{{SampleIndex: 160, Label: "T1", DurationSeconds: 4.1}, {SampleIndex: 175, Label: "T2"}}, true},
		{"before the batch", []*neurov1.Marker{{SampleIndex: 159, Label: "T1"}}, false},
		{"after the batch", []*neurov1.Marker{{SampleIndex: 176, Label: "T1"}}, false},
		{"empty label", []*neurov1.Marker{{SampleIndex: 160}}, false},
		{"long label", []*neurov1.Marker{{SampleIndex: 160, Label: strings.Repeat("a", maxMarkerLabelBytes+1)}}, false},
		{"negative duration", []*neurov1.Marker{{SampleIndex: 160, Label: "T1", DurationSeconds: -1}}, false},
		{"too many", many, false},
	}
	for _, c := range cases {
		if err := validateBatch(header, batch(c.markers...)); (err == nil) != c.ok {
			t.Errorf("%s: err = %v, want ok = %v", c.name, err, c.ok)
		}
	}
}
