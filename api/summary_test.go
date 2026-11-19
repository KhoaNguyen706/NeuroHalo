package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go/v2"
	"github.com/anthropics/anthropic-sdk-go/v2/option"
)

// TestBadEpisodes checks the stretches built from bad and recovered events:
// a recovery closes a stretch, a new reason starts a new one, and a sensor
// still bad at the end stays open.
func TestBadEpisodes(t *testing.T) {
	events := []eventRow{
		{Type: "marker", At: 1},
		{Type: "channel_bad", Channel: "Cz", Reason: "flat", At: 23},
		{Type: "channel_bad", Channel: "C3", Reason: "noisy", At: 72},
		{Type: "channel_recovered", Channel: "C3", At: 74},
		{Type: "channel_bad", Channel: "Cz", Reason: "noisy", At: 90},
		{Type: "channel_recovered", Channel: "O1", At: 95}, // never bad: ignored
	}
	got, now := badEpisodes(events)
	if len(got) != 3 {
		t.Fatalf("%d stretches, want 3: %+v", len(got), got)
	}
	if got[0].Channel != "Cz" || got[0].Reason != "flat" || got[0].To == nil || *got[0].To != 90 {
		t.Errorf("first stretch %+v, want Cz flat 23 to 90", got[0])
	}
	if got[1].Channel != "C3" || got[1].To == nil || *got[1].To != 74 {
		t.Errorf("second stretch %+v, want C3 72 to 74", got[1])
	}
	if got[2].Channel != "Cz" || got[2].Reason != "noisy" || got[2].To != nil {
		t.Errorf("third stretch %+v, want Cz noisy, still open", got[2])
	}
	if len(now) != 1 || now[0] != "Cz" {
		t.Errorf("bad at the end %v, want [Cz]", now)
	}
}

// TestRMSSMEAfter checks that only the windows from the marker on count, and
// that windows without an SME are skipped.
func TestRMSSMEAfter(t *testing.T) {
	e := erpJSON{Bins: []float64{-0.2, -0.1, 0, 0.1, 0.2}}
	// One sensor: 100 before the marker (ignored), then 3, 4 and none.
	l := erpLabelJSON{SME: []float32{100, 100, 3, 4, -1}}
	if got, want := rmsSMEAfter(e, l, 0, 1), math.Sqrt((9+16)/2.0); math.Abs(got-want) > 1e-9 {
		t.Errorf("rms %v, want %v", got, want)
	}
	if got := rmsSMEAfter(e, erpLabelJSON{SME: []float32{1, 1, -1, -1, -1}}, 0, 1); got != -1 {
		t.Errorf("rms without SMEs %v, want -1", got)
	}
}

// TestFactsText checks that the text Claude reads names what matters: bad
// sensors with their times, the counts, and each label's trials and SME.
func TestFactsText(t *testing.T) {
	to := 74.0
	f := factsJSON{
		Session: sessionJSON{ID: "s-1", Label: "Left and right fist", Source: "sample", Status: "ended",
			SampleRateHz: 160, ChannelNames: make([]string, 64), StartedAt: time.Unix(0, 0)},
		Seconds:     125,
		BadEpisodes: []badEpisode{{Channel: "C3", Reason: "noisy", From: 72, To: &to}, {Channel: "Cz", Reason: "flat", From: 100}},
		BadNow:      []string{"Cz"},
		Eye:         25,
		Markers:     []markerCount{{"T0", 15}, {"T1", 8}},
		ERPSensor:   "Cz",
		ERP:         []erpFact{{Label: "T1", Trials: 8, KeptAt100: 0, KeptAt200: 1, SMEAt200: -1, SMEAll: 12.34}},
	}
	text := factsText(f)
	for _, want := range []string{
		`"Left and right fist"`, "2:05 long, ended", "64 sensors at 160 Hz",
		"C3 noisy from 1:12 to 1:14 (0:02)", "Cz flat from 1:40 to the end (0:25)", "Still bad at the end: Cz.",
		"Eye movements: 25 (12.0 per minute)", "Markers: T0 15, T1 8.",
		"T1: 8 trials; kept 0 under 100 uV and 1 under 200 uV; no SME (fewer than 2 trials) at 200 uV, SME 12.3 uV keeping all",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("facts text lacks %q:\n%s", want, text)
		}
	}
}

// TestClaudeSummarizer runs the Claude client against a fake Messages API:
// it checks the request (model, effort, refusal fallback, the facts as the
// user turn) and that the reply's text and model are read back.
func TestClaudeSummarizer(t *testing.T) {
	var got map[string]any
	var beta string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		beta = r.Header.Get("anthropic-beta")
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("request body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","model":"claude-opus-5-5",
			"content":[{"type":"text","text":"  Usable for rhythms.  "}],
			"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":5}}`)
	}))
	defer srv.Close()
	c := anthropic.NewClient(option.WithAPIKey("test"), option.WithBaseURL(srv.URL), option.WithMaxRetries(0))
	body, model, err := (&claudeSummarizer{client: &c}).summarize(context.Background(), "Session: test")
	if err != nil {
		t.Fatal(err)
	}
	if body != "Usable for rhythms." || model != "claude-opus-5-5" {
		t.Errorf("got %q by %q", body, model)
	}
	if got["model"] != summaryModel || got["fallbacks"] != "default" {
		t.Errorf("model %v, fallbacks %v", got["model"], got["fallbacks"])
	}
	if oc, _ := got["output_config"].(map[string]any); oc["effort"] != "low" {
		t.Errorf("output_config %v, want effort low", got["output_config"])
	}
	if !strings.Contains(beta, "server-side-fallback-2026-07-01") {
		t.Errorf("anthropic-beta %q lacks the fallback beta", beta)
	}
	msgs, _ := got["messages"].([]any)
	if len(msgs) != 1 || !strings.Contains(fmt.Sprint(msgs[0]), "Session: test") {
		t.Errorf("messages %v, want the facts as the one user turn", msgs)
	}

	if _, _, err := (&claudeSummarizer{}).summarize(context.Background(), "x"); !errors.Is(err, errNoKey) {
		t.Errorf("without a key: %v, want errNoKey", err)
	}
}
