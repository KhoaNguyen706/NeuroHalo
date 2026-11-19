package main

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go/v2"
	"github.com/anthropics/anthropic-sdk-go/v2/option"
	"github.com/jackc/pgx/v5"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// The session summary (build step 11): the api gathers what it knows about a
// session (sensors that went bad and when, eye movements and muscle activity,
// markers, and how precise each ERP is) as plain facts, and Claude turns them
// into a short note on whether the recording is usable and what to do next.
// The facts are also served on their own (GET /sessions/{id}/facts), which
// is what the MCP server hands to an agent.

// summaryModel is the Claude model that writes summaries.
const summaryModel = "claude-opus-5-5"

// erpFactSensor is the sensor the ERP facts are given for, as on the session
// page when no sensor is highlighted.
const erpFactSensor = "Cz"

// badEpisode is one stretch of time a sensor was marked bad.
// Built by: badEpisodes. Part of: factsJSON.
type badEpisode struct {
	Channel string   `json:"channel"`
	Reason  string   `json:"reason"` // "flat" or "noisy"
	From    float64  `json:"from"`   // seconds into the session
	To      *float64 `json:"to"`     // null when still bad at the end
}

// markerCount is how many markers of one label the session has.
// Part of: factsJSON.
type markerCount struct {
	Label string `json:"label"`
	Count int    `json:"count"`
}

// erpFact is how usable one label's ERP is at erpFactSensor.
// Built by: erpFacts. Part of: factsJSON.
type erpFact struct {
	Label     string  `json:"label"`
	Trials    int     `json:"trials"`    // markers with a full stretch stored
	KeptAt100 int     `json:"keptAt100"` // trials under 100 µV peak-to-peak at the sensor
	KeptAt200 int     `json:"keptAt200"`
	SMEAt200  float64 `json:"smeAt200"` // RMS SME after the marker, µV, with the 200 µV rule; -1 below 2 trials
	SMEAll    float64 `json:"smeAll"`   // the same, keeping every trial; -1 below 2 trials
}

// factsJSON is everything the summary is written from.
// Built by: api.sessionFacts. Used by: getFacts, createSummary, factsText.
type factsJSON struct {
	Session      sessionJSON   `json:"session"`
	Study        string        `json:"study"`   // the study's name, "" when not filed
	Seconds      float64       `json:"seconds"` // how long the session ran (so far)
	BadEpisodes  []badEpisode  `json:"badEpisodes"`
	BadNow       []string      `json:"badNow"` // sensors still bad at the end (or now)
	Eye          int           `json:"eye"`
	Muscle       int           `json:"muscle"`
	AlphaRises   int           `json:"alphaRises"`
	Markers      []markerCount `json:"markers"` // by label
	ERPSensor    string        `json:"erpSensor"` // "" when the session has no erpFactSensor
	ERP          []erpFact     `json:"erp"` // empty without markers or without erpFactSensor
	Text         string        `json:"text"` // the facts as the plain text Claude reads
}

// summaryFailures counts Claude calls for a summary that failed.
// Incremented by: createSummary.
var summaryFailures = promauto.NewCounter(prometheus.CounterOpts{
	Name: "summary_failures_total",
	Help: "Session summaries that could not be written because the Claude call failed.",
})

// summaryJSON is a written summary.
// Returned by: getSummary, createSummary.
type summaryJSON struct {
	Body      string    `json:"body"`
	Model     string    `json:"model"`
	CreatedAt time.Time `json:"createdAt"`
	Final     bool      `json:"final"` // written after the session ended, so it is kept
}

// summarizer writes a summary from the facts text. The real one calls Claude;
// tests use a fake.
// Implemented by: claudeSummarizer. Used by: createSummary (via api.summarizer).
type summarizer interface {
	summarize(ctx context.Context, facts string) (body, model string, err error)
}

// errNoKey is returned by summarize when no Anthropic credential is set.
var errNoKey = errors.New("no ANTHROPIC_API_KEY is set for the api service")

// claudeSummarizer calls Claude through the Anthropic SDK.
// Created by: newSummarizer.
type claudeSummarizer struct {
	client *anthropic.Client // nil when no key is set
}

// newSummarizer returns the Claude summarizer. Without ANTHROPIC_API_KEY (or
// ANTHROPIC_AUTH_TOKEN) it still returns one, whose summarize answers errNoKey,
// so the rest of the api runs without a key.
// Used by: main.
func newSummarizer() *claudeSummarizer {
	if os.Getenv("ANTHROPIC_API_KEY") == "" && os.Getenv("ANTHROPIC_AUTH_TOKEN") == "" {
		return &claudeSummarizer{}
	}
	c := anthropic.NewClient(option.WithRequestTimeout(2 * time.Minute))
	return &claudeSummarizer{client: &c}
}

// summarySystem is the standing instruction for every summary. It does not
// change between requests.
const summarySystem = `You write the end-of-session note for NeuroHalo, a live EEG monitor and recorder used in research labs. The reader is the researcher who ran the session, often a student, deciding whether the recording is good enough to keep or whether to record again while the participant is still there.

You are given facts the system measured. Write from those facts only. Do not guess at causes you cannot see in them, and do not say anything about the participant's health, brain or mental state: this is research data quality, not diagnosis.

How the facts were measured:
- Every sensor is checked twice a second over the last 2 seconds: flat (less than 0.5 uV of variation) or noisy (more than 500 uV peak to peak). It is marked bad after 3 such checks in a row and recovered after 3 good ones. A front sensor that only carries a blink is not marked bad.
- Eye movements are large swings at the sensors above the eyes; muscle activity is a sensor's power being mostly above 30 Hz. Both are normal in small numbers and are usually removed later in analysis.
- The ERP is the voltage averaged after each marker label, 30 Hz low-pass, -200 to 800 ms, trials over the threshold left out per sensor. The SME is the standard error of the mean voltage in 100 ms windows after the marker, in uV: smaller is more precise. As a rough guide, a few uV is good for most ERP studies; above 10 uV the average is hard to use.

Write plain text, no headings, no bullet lists, no markdown, at most 150 words:
1. One sentence: is this recording usable, and for what (rhythms or ERPs)?
2. The problems that matter, with sensor names and times, most important first. Leave out what does not matter.
3. What to do next, concretely (for example re-seat a sensor, record more trials of a label, keep it as is).
Explain any technical term in a few plain words the first time.`

// summarize asks Claude for the summary. It returns the text and the model
// that wrote it (a server-side fallback may answer instead of summaryModel).
func (c *claudeSummarizer) summarize(ctx context.Context, facts string) (string, string, error) {
	if c.client == nil {
		return "", "", errNoKey
	}
	resp, err := c.client.Beta.Messages.New(ctx, anthropic.BetaMessageNewParams{
		Model:     summaryModel,
		MaxTokens: 16000,
		// A short summary of given facts does not need long thinking.
		OutputConfig: anthropic.BetaOutputConfigParam{Effort: anthropic.BetaOutputConfigEffortLow},
		// If Claude declines the request, the API retries it on a fallback model.
		Betas:     []anthropic.AnthropicBeta{anthropic.AnthropicBetaServerSideFallback2026_07_01},
		Fallbacks: anthropic.BetaFallbacksParamOfDefault(),
		System:    []anthropic.BetaTextBlockParam{{Text: summarySystem}},
		Messages: []anthropic.BetaMessageParam{
			anthropic.NewBetaUserMessage(anthropic.NewBetaTextBlock(facts)),
		},
	})
	if err != nil {
		return "", "", err
	}
	if resp.StopReason == anthropic.BetaStopReasonRefusal {
		return "", "", fmt.Errorf("Claude declined to summarize this session")
	}
	var text strings.Builder
	for _, block := range resp.Content {
		if b, ok := block.AsAny().(anthropic.BetaTextBlock); ok {
			text.WriteString(b.Text)
		}
	}
	body := strings.TrimSpace(text.String())
	if body == "" {
		return "", "", fmt.Errorf("Claude returned no text (stop reason %s)", resp.StopReason)
	}
	return body, string(resp.Model), nil
}

// eventRow is one stored event, as badEpisodes reads it.
// Read by: api.sessionFacts.
type eventRow struct {
	Type, Channel string
	At            float64 // seconds into the session
	Reason        string  // channel_bad only
}

// badEpisodes turns channel_bad and channel_recovered events, in time order,
// into stretches per sensor. A channel_bad for a sensor already bad (its
// reason changed) ends the open stretch and starts a new one. Stretches still
// open at the end have To nil. Returns them by start time, and the sensors
// still bad, sorted.
// Used by: api.sessionFacts.
func badEpisodes(events []eventRow) ([]badEpisode, []string) {
	open := map[string]int{} // channel -> index in out
	out := []badEpisode{}
	for _, e := range events {
		switch e.Type {
		case "channel_bad":
			if i, ok := open[e.Channel]; ok {
				at := e.At
				out[i].To = &at
			}
			open[e.Channel] = len(out)
			out = append(out, badEpisode{Channel: e.Channel, Reason: e.Reason, From: e.At})
		case "channel_recovered":
			if i, ok := open[e.Channel]; ok {
				at := e.At
				out[i].To = &at
				delete(open, e.Channel)
			}
		}
	}
	now := []string{}
	for c := range open {
		now = append(now, c)
	}
	sort.Strings(now)
	return out, now
}

// rmsSMEAfter is the root mean square of a sensor's SMEs in the windows that
// start at or after the marker, as the ERP chart's "±12 µV" note; -1 when
// none has an SME.
// Used by: erpFacts.
func rmsSMEAfter(e erpJSON, l erpLabelJSON, c, channels int) float64 {
	var sum float64
	n := 0
	for b, start := range e.Bins {
		if start < -1e-9 {
			continue
		}
		if s := float64(l.SME[b*channels+c]); s >= 0 {
			sum += s * s
			n++
		}
	}
	if n == 0 {
		return -1
	}
	return math.Sqrt(sum / float64(n))
}

// erpFacts gives, per label, how many trials sensor c keeps at 100 µV, at
// 200 µV and with all trials, and the SMEs. at100, at200 and all are the
// session's ERPs at those thresholds.
// Used by: api.sessionFacts.
func erpFacts(at100, at200, all erpJSON, c, channels int) []erpFact {
	out := []erpFact{}
	for i, l := range all.Labels {
		f := erpFact{Label: l.Label, Trials: l.Trials, SMEAll: rmsSMEAfter(all, l, c, channels), SMEAt200: -1}
		if i < len(at100.Labels) && at100.Labels[i].Label == l.Label {
			f.KeptAt100 = at100.Labels[i].Good[c]
		}
		if i < len(at200.Labels) && at200.Labels[i].Label == l.Label {
			f.KeptAt200 = at200.Labels[i].Good[c]
			f.SMEAt200 = rmsSMEAfter(at200, at200.Labels[i], c, channels)
		}
		out = append(out, f)
	}
	return out
}

// smeText writes an SME for the facts text.
func smeText(v float64) string {
	if v < 0 {
		return "no SME (fewer than 2 trials)"
	}
	return fmt.Sprintf("SME %.1f uV", v)
}

// factsText writes the facts as the plain text Claude reads.
// Used by: api.sessionFacts.
func factsText(f factsJSON) string {
	var b strings.Builder
	s := f.Session
	name := s.Label
	if name == "" {
		name = s.ID
	}
	state := "ended"
	if s.Status != "ended" {
		state = "still recording"
	}
	fmt.Fprintf(&b, "Session: %q (%s), %s long, %s. %d sensors at %g Hz.\n",
		name, s.Source, clockText(f.Seconds), state, len(s.ChannelNames), s.SampleRateHz)
	if f.Study != "" {
		fmt.Fprintf(&b, "Study %q, participant %s, task %s", f.Study, s.Participant, s.Task)
		if s.Visit != "" {
			fmt.Fprintf(&b, ", visit %s", s.Visit)
		}
		b.WriteString(".\n")
	}
	if s.Recording != nil {
		fmt.Fprintf(&b, "Raw recording file: %s, %s of signal.\n", s.Recording.Status, clockText(s.Recording.Seconds))
	}

	if len(f.BadEpisodes) == 0 {
		b.WriteString("Bad sensors: none during the whole session.\n")
	} else {
		fmt.Fprintf(&b, "Bad sensors (%d stretches):\n", len(f.BadEpisodes))
		for _, e := range f.BadEpisodes {
			if e.To == nil {
				fmt.Fprintf(&b, "- %s %s from %s to the end (%s)\n", e.Channel, e.Reason, clockText(e.From),
					clockText(f.Seconds-e.From))
			} else {
				fmt.Fprintf(&b, "- %s %s from %s to %s (%s)\n", e.Channel, e.Reason, clockText(e.From), clockText(*e.To),
					clockText(*e.To-e.From))
			}
		}
		if len(f.BadNow) == 0 {
			b.WriteString("All sensors were good again at the end.\n")
		} else {
			fmt.Fprintf(&b, "Still bad at the end: %s.\n", strings.Join(f.BadNow, ", "))
		}
	}

	perMin := func(n int) string {
		if f.Seconds < 30 {
			return fmt.Sprintf("%d", n)
		}
		return fmt.Sprintf("%d (%.1f per minute)", n, float64(n)/(f.Seconds/60))
	}
	fmt.Fprintf(&b, "Eye movements: %s. Muscle activity: %s. Alpha rises over the back of the head: %d.\n",
		perMin(f.Eye), perMin(f.Muscle), f.AlphaRises)

	if len(f.Markers) == 0 {
		b.WriteString("Markers: none, so there are no trials and no ERP.\n")
		return b.String()
	}
	b.WriteString("Markers:")
	for i, m := range f.Markers {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, " %s %d", m.Label, m.Count)
	}
	b.WriteString(".\n")
	switch {
	case f.ERPSensor == "":
		fmt.Fprintf(&b, "ERP: not given, the session has no %s sensor.\n", erpFactSensor)
		return b.String()
	case len(f.ERP) == 0:
		b.WriteString("ERP: no trial yet; a marker needs 2 s of signal before it and 4 s after it to count.\n")
		return b.String()
	}
	fmt.Fprintf(&b, "ERP at %s, per label:\n", f.ERPSensor)
	for _, e := range f.ERP {
		fmt.Fprintf(&b, "- %s: %d trials; kept %d under 100 uV and %d under 200 uV; %s at 200 uV, %s keeping all\n",
			e.Label, e.Trials, e.KeptAt100, e.KeptAt200, smeText(e.SMEAt200), smeText(e.SMEAll))
	}
	return b.String()
}

// sessionFacts gathers the facts of one session. It returns errNoSession for
// an unknown session.
// Used by: getFacts, createSummary.
func (a *api) sessionFacts(ctx context.Context, id string) (factsJSON, error) {
	var f factsJSON
	var err error
	f.Session, err = scanSession(a.db.QueryRow(ctx, sessionSelect+` WHERE s.id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return f, errNoSession
	} else if err != nil {
		return f, err
	}
	if f.Session.StudyID != "" {
		if err := a.db.QueryRow(ctx, `SELECT name FROM studies WHERE id = $1`, f.Session.StudyID).Scan(&f.Study); err != nil &&
			!errors.Is(err, pgx.ErrNoRows) {
			return f, err
		}
	}
	end := time.Now()
	if f.Session.EndedAt != nil {
		end = *f.Session.EndedAt
	}
	f.Seconds = max(0, end.Sub(f.Session.StartedAt).Seconds())

	rows, err := a.db.Query(ctx, `
		SELECT type, channel, EXTRACT(EPOCH FROM at - $2::timestamptz)::float8,
			COALESCE(details->>'reason', ''), COALESCE(details->>'label', '')
		FROM events WHERE session_id = $1 ORDER BY at, id`, id, f.Session.StartedAt)
	if err != nil {
		return f, err
	}
	var events []eventRow
	markers := map[string]int{}
	for rows.Next() {
		var e eventRow
		var label string
		if err := rows.Scan(&e.Type, &e.Channel, &e.At, &e.Reason, &label); err != nil {
			rows.Close()
			return f, err
		}
		switch e.Type {
		case "eye_movement":
			f.Eye++
		case "muscle":
			f.Muscle++
		case "alpha_rise":
			f.AlphaRises++
		case "marker":
			markers[label]++
		}
		events = append(events, e)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return f, err
	}
	f.BadEpisodes, f.BadNow = badEpisodes(events)
	f.Markers = []markerCount{}
	for label, n := range markers {
		f.Markers = append(f.Markers, markerCount{label, n})
	}
	sort.Slice(f.Markers, func(i, j int) bool { return f.Markers[i].Label < f.Markers[j].Label })

	f.ERP = []erpFact{}
	sensor := -1
	for i, n := range f.Session.ChannelNames {
		if strings.EqualFold(n, erpFactSensor) {
			sensor = i
		}
	}
	if len(markers) > 0 && sensor >= 0 {
		var erps [3]erpJSON
		for i, reject := range []float64{100, 200, 0} {
			if erps[i], err = a.computeERP(ctx, id, reject); err != nil {
				return f, err
			}
		}
		f.ERPSensor = f.Session.ChannelNames[sensor]
		f.ERP = erpFacts(erps[0], erps[1], erps[2], sensor, len(f.Session.ChannelNames))
	}
	f.Text = factsText(f)
	return f, nil
}

// getFacts handles GET /sessions/{id}/facts: what the summary is written
// from, as data and as text. Free and instant; the MCP server uses it.
func (a *api) getFacts(w http.ResponseWriter, r *http.Request) {
	f, err := a.sessionFacts(r.Context(), r.PathValue("id"))
	switch {
	case errors.Is(err, errNoSession):
		writeError(w, http.StatusNotFound, "no such session")
	case err != nil:
		serverError(w, r, err)
	default:
		writeJSON(w, http.StatusOK, f)
	}
}

// readSummary returns the stored summary of a session; ok is false when
// there is none.
// Used by: getSummary, createSummary.
func (a *api) readSummary(ctx context.Context, id string) (s summaryJSON, ok bool, err error) {
	err = a.db.QueryRow(ctx, `SELECT body, model, created_at FROM summaries WHERE session_id = $1`, id).
		Scan(&s.Body, &s.Model, &s.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return s, false, nil
	}
	s.Final = true
	return s, err == nil, err
}

// getSummary handles GET /sessions/{id}/summary: the stored summary, or 404
// when none was written after the session ended. It never calls Claude.
func (a *api) getSummary(w http.ResponseWriter, r *http.Request) {
	s, ok, err := a.readSummary(r.Context(), r.PathValue("id"))
	switch {
	case err != nil:
		serverError(w, r, err)
	case !ok:
		writeError(w, http.StatusNotFound, "no summary yet")
	default:
		writeJSON(w, http.StatusOK, s)
	}
}

// createSummary handles POST /sessions/{id}/summary: returns the stored
// summary if there is one (unless ?fresh=1), otherwise asks Claude for one.
// A summary written after the session ended is stored; one written while it
// is still recording is only returned, since the facts are still changing.
// Answers 503 when no Anthropic key is set, 502 when the Claude call fails.
func (a *api) createSummary(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := r.PathValue("id")
	if r.URL.Query().Get("fresh") != "1" {
		if s, ok, err := a.readSummary(ctx, id); err != nil {
			serverError(w, r, err)
			return
		} else if ok {
			writeJSON(w, http.StatusOK, s)
			return
		}
	}
	f, err := a.sessionFacts(ctx, id)
	switch {
	case errors.Is(err, errNoSession):
		writeError(w, http.StatusNotFound, "no such session")
		return
	case err != nil:
		serverError(w, r, err)
		return
	}
	body, model, err := a.summarizer.summarize(ctx, f.Text)
	switch {
	case errors.Is(err, errNoKey):
		writeError(w, http.StatusServiceUnavailable,
			"summaries need an Anthropic API key: set ANTHROPIC_API_KEY for the api service and restart it")
		return
	case err != nil:
		summaryFailures.Inc()
		writeError(w, http.StatusBadGateway, "the summary could not be written: "+err.Error())
		return
	}
	out := summaryJSON{Body: body, Model: model, CreatedAt: time.Now().UTC(), Final: f.Session.Status == "ended"}
	if out.Final {
		if _, err := a.db.Exec(ctx, `
			INSERT INTO summaries (session_id, body, model, created_at) VALUES ($1, $2, $3, $4)
			ON CONFLICT (session_id) DO UPDATE SET body = EXCLUDED.body, model = EXCLUDED.model,
				created_at = EXCLUDED.created_at`, id, body, model, out.CreatedAt); err != nil {
			serverError(w, r, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, out)
}
