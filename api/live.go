package main

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/twmb/franz-go/pkg/kgo"
	"google.golang.org/protobuf/proto"

	"github.com/neurohalo/neurohalo/internal/eeg"

	neurov1 "github.com/neurohalo/neurohalo/gen/neuro/v1"
)

const (
	viewerEventBuffer = 64
	viewerRawBuffer   = 32 // raw batches waiting for a slow viewer: about 3 seconds
	maxRawChannels    = 8  // sensors one viewer may watch as raw signal at once
	rawTopic          = "eeg.raw"
	wsWriteTimeout    = 5 * time.Second
	wsPingEvery       = 30 * time.Second
)

// pipelineLatencySeconds is how long a frame took to get here: from the
// moment the sender sent the newest batch in its window, through the gateway,
// both Kafka hops and the processor.
// Observed by: runLiveConsumer.
var pipelineLatencySeconds = promauto.NewHistogram(prometheus.HistogramOpts{
	Name:    "pipeline_latency_seconds",
	Help:    "Time from a batch being sent to its feature frame reaching the api.",
	Buckets: prometheus.ExponentialBuckets(0.001, 2, 15), // 1 ms to about 16 s
})

// registerLiveMetrics exposes two numbers the api already keeps: open
// WebSockets (ws_clients) and frames skipped for slow viewers
// (ws_frames_dropped_total).
// Used by: main.
func registerLiveMetrics(a *api) {
	promauto.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "ws_clients",
		Help: "WebSocket viewers connected right now.",
	}, func() float64 { return float64(a.wsClients.Load()) })
	promauto.NewCounterFunc(prometheus.CounterOpts{
		Name: "ws_frames_dropped_total",
		Help: "Frames replaced by a newer one before a slow viewer took them.",
	}, func() float64 { return float64(a.hub.framesDropped.Load()) })
}

// viewer is one open WebSocket. It holds at most one frame: a slow viewer
// skips to the newest frame and never slows the others down.
// Created by: api.liveSession.
// Used by: hub.add, hub.remove, hub.publishFrame, hub.publishEvent,
// offerFrame (its method), api.liveSession's write loop.
type viewer struct {
	band  atomic.Int32 // index into eeg.Bands
	links atomic.Bool  // subscribed with "links": true
	// The channel indexes subscribed to as raw signal; nil when there are none.
	rawChannels atomic.Pointer[[]int]
	frames      chan *neurov1.FeatureFrame
	events      chan *neurov1.Event
	raw         chan *neurov1.SampleBatch
}

// newViewer returns a viewer watching alpha until it subscribes to another band.
func newViewer() *viewer {
	v := &viewer{
		frames: make(chan *neurov1.FeatureFrame, 1),
		events: make(chan *neurov1.Event, viewerEventBuffer),
		raw:    make(chan *neurov1.SampleBatch, viewerRawBuffer),
	}
	v.band.Store(eeg.AlphaIndex)
	return v
}

// offerFrame puts f in the viewer's one-frame slot, replacing an unsent older
// frame. It returns false when an older frame was thrown away. Only the live
// consumer goroutine calls it, so the empty-then-fill step cannot race.
func (v *viewer) offerFrame(f *neurov1.FeatureFrame) bool {
	select {
	case v.frames <- f:
		return true
	default:
	}
	select {
	case <-v.frames:
	default:
	}
	select {
	case v.frames <- f:
	default:
	}
	return false
}

// hub knows which viewers watch which session and hands them frames and events.
// Created by: main.
// Used by: add, remove, publishFrame, publishEvent (its methods),
// runLiveConsumer, api.liveSession.
type hub struct {
	mu            sync.Mutex
	viewers       map[string]map[*viewer]struct{}
	framesDropped atomic.Uint64
	eventsDropped atomic.Uint64
	rawDropped    atomic.Uint64
}

// newHub returns a hub with no viewers.
func newHub() *hub {
	return &hub{viewers: map[string]map[*viewer]struct{}{}}
}

// add registers a viewer for one session.
func (h *hub) add(sessionID string, v *viewer) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.viewers[sessionID] == nil {
		h.viewers[sessionID] = map[*viewer]struct{}{}
	}
	h.viewers[sessionID][v] = struct{}{}
}

// remove forgets a viewer when its WebSocket closes.
func (h *hub) remove(sessionID string, v *viewer) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.viewers[sessionID], v)
	if len(h.viewers[sessionID]) == 0 {
		delete(h.viewers, sessionID)
	}
}

// watchers returns a copy of the session's viewers, so sending happens outside the lock.
func (h *hub) watchers(sessionID string) []*viewer {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]*viewer, 0, len(h.viewers[sessionID]))
	for v := range h.viewers[sessionID] {
		out = append(out, v)
	}
	return out
}

// publishFrame offers a frame to every viewer of its session.
func (h *hub) publishFrame(f *neurov1.FeatureFrame) {
	for _, v := range h.watchers(f.SessionId) {
		if !v.offerFrame(f) {
			h.framesDropped.Add(1)
		}
	}
}

// publishEvent queues an event for every viewer of its session. A viewer
// whose queue is full misses it; the page can always reload events over REST.
func (h *hub) publishEvent(e *neurov1.Event) {
	for _, v := range h.watchers(e.SessionId) {
		select {
		case v.events <- e:
		default:
			h.eventsDropped.Add(1)
		}
	}
}

// wantsRaw reports whether any viewer of the session subscribed to raw
// signal. The live consumer asks before it decodes a raw batch, so sessions
// nobody watches in detail cost almost nothing.
func (h *hub) wantsRaw(sessionID string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for v := range h.viewers[sessionID] {
		if v.rawChannels.Load() != nil {
			return true
		}
	}
	return false
}

// publishRaw queues a raw batch for every viewer of its session that
// subscribed to raw signal. A viewer whose queue is full misses the batch,
// which shows as a short gap in its trace and never slows the others.
func (h *hub) publishRaw(b *neurov1.SampleBatch) {
	for _, v := range h.watchers(b.SessionId) {
		if v.rawChannels.Load() == nil {
			continue
		}
		select {
		case v.raw <- b:
		default:
			h.rawDropped.Add(1)
		}
	}
}

// runLiveConsumer reads every partition of eeg.features and eeg.events from
// the newest message on and passes each one to the hub. It uses no consumer
// group, so every api instance sees every frame, and it commits nothing,
// because a viewer only ever wants what is happening now.
func runLiveConsumer(ctx context.Context, brokers []string, h *hub) error {
	cl, err := kgo.NewClient(
		kgo.SeedBrokers(brokers...),
		kgo.ConsumeTopics(featuresTopic, eventsTopic, rawTopic),
		kgo.ConsumeResetOffset(kgo.NewOffset().AtEnd()),
	)
	if err != nil {
		return err
	}
	defer cl.Close()
	log.Printf("live consumer started, reading %s, %s and %s from the end", featuresTopic, eventsTopic, rawTopic)
	for {
		fetches := cl.PollFetches(ctx)
		if ctx.Err() != nil || fetches.IsClientClosed() {
			return nil
		}
		fetches.EachError(func(topic string, p int32, err error) {
			log.Printf("live fetch %s/%d: %v", topic, p, err)
		})
		fetches.EachRecord(func(rec *kgo.Record) {
			if rec.Topic == rawTopic {
				// Every message is keyed by session ID, so the key says who wants it before any decoding.
				if !h.wantsRaw(string(rec.Key)) {
					return
				}
				var b neurov1.SampleBatch
				if proto.Unmarshal(rec.Value, &b) == nil {
					h.publishRaw(&b)
				}
				return
			}
			if rec.Topic == eventsTopic {
				var e neurov1.Event
				if proto.Unmarshal(rec.Value, &e) == nil {
					h.publishEvent(&e)
				}
				return
			}
			var f neurov1.FeatureFrame
			if proto.Unmarshal(rec.Value, &f) == nil {
				pipelineLatencySeconds.Observe(time.Since(time.Unix(0, f.NewestSentTimeUnixNanos)).Seconds())
				h.publishFrame(&f)
			}
		})
	}
}

// wsFrame is the JSON a viewer receives every 0.5 s.
// Built by: frameMessage.
// Used by: api.liveSession.
type wsFrame struct {
	Type        string    `json:"type"`
	WindowIndex uint64    `json:"windowIndex"`
	SentTimeMs  int64     `json:"sentTimeMs"`
	Band        string    `json:"band"`
	Power       []float32 `json:"power"`
	Quality     []int32   `json:"quality"`
}

// wsLinks is the JSON a viewer subscribed with "links": true receives right
// after each frame: the window's strongest sensor pairs as [a, b, strength].
// Built by: linksMessage.
// Used by: api.liveSession.
type wsLinks struct {
	Type        string       `json:"type"`
	WindowIndex uint64       `json:"windowIndex"`
	Pairs       [][3]float32 `json:"pairs"`
}

// linksMessage turns a frame's links into the JSON sent to links viewers.
// Pairs is never null, so an empty window clears the viewer's lines.
func linksMessage(f *neurov1.FeatureFrame) wsLinks {
	msg := wsLinks{Type: "links", WindowIndex: f.WindowIndex, Pairs: make([][3]float32, 0, len(f.Links))}
	for _, l := range f.Links {
		msg.Pairs = append(msg.Pairs, [3]float32{float32(l.A), float32(l.B), l.Strength})
	}
	return msg
}

// wsRaw is the JSON a viewer subscribed with "raw": [channels] receives for
// each 100 ms batch: the samples of those channels, in microvolts.
// Built by: rawMessage.
// Used by: api.liveSession.
type wsRaw struct {
	Type        string      `json:"type"`
	FirstSample uint64      `json:"firstSample"` // position of samples[..][0] in the recording
	Channels    []int       `json:"channels"`    // channel indexes, as subscribed
	Samples     [][]float32 `json:"samples"`     // one row per channel in Channels
}

// rawMessage cuts the subscribed channels out of a raw batch. A channel
// index the session does not have gets an empty row.
func rawMessage(b *neurov1.SampleBatch, channels []int) wsRaw {
	msg := wsRaw{Type: "raw", FirstSample: b.FirstSampleIndex, Channels: channels, Samples: make([][]float32, len(channels))}
	n := int(b.NumSamples)
	if n == 0 || len(b.Values)%n != 0 {
		return msg
	}
	width := len(b.Values) / n // channels in the session
	for i, c := range channels {
		row := make([]float32, 0, n)
		if c < width {
			for s := 0; s < n; s++ {
				row = append(row, b.Values[s*width+c])
			}
		}
		msg.Samples[i] = row
	}
	return msg
}

// rawSubscription cleans the channel list of a subscribe message: at most
// maxRawChannels, no negative indexes, no repeats. It returns nil for an
// empty list, which turns raw signal off.
func rawSubscription(channels []int) *[]int {
	seen := map[int]bool{}
	out := []int{}
	for _, c := range channels {
		if c >= 0 && !seen[c] && len(out) < maxRawChannels {
			seen[c] = true
			out = append(out, c)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return &out
}

// wsEvent is the JSON a viewer receives for each event.
// Built by: api.liveSession.
type wsEvent struct {
	Type      string          `json:"type"`
	EventType string          `json:"eventType"`
	Channel   string          `json:"channel"`
	AtMs      int64           `json:"atMs"`
	Details   json.RawMessage `json:"details"`
}

// frameMessage cuts one band out of a frame: one power value and one quality
// per channel, in the session's channel order.
func frameMessage(f *neurov1.FeatureFrame, band int) wsFrame {
	n := len(f.Quality)
	msg := wsFrame{
		Type:        "frame",
		WindowIndex: f.WindowIndex,
		SentTimeMs:  f.NewestSentTimeUnixNanos / int64(time.Millisecond),
		Band:        eeg.Bands[band].Name,
		Power:       make([]float32, n),
		Quality:     make([]int32, n),
	}
	for c := 0; c < n && c*eeg.NumBands+band < len(f.BandPower); c++ {
		msg.Power[c] = f.BandPower[c*eeg.NumBands+band]
		msg.Quality[c] = int32(f.Quality[c])
	}
	return msg
}

// bandIndex finds a band by name ("alpha" -> 2); ok is false for an unknown name.
func bandIndex(name string) (int, bool) {
	for i, b := range eeg.Bands {
		if b.Name == name {
			return i, true
		}
	}
	return 0, false
}

// originPatterns turns WEB_ORIGIN ("http://localhost:3000,https://x.dev")
// into the host patterns the WebSocket library checks the Origin header against.
func originPatterns(origins []string) []string {
	var out []string
	for _, o := range origins {
		if u, err := url.Parse(o); err == nil && u.Host != "" {
			out = append(out, u.Host)
		}
	}
	return out
}

// liveSession handles WS /ws/sessions/{id}/live. The viewer sends
// {"type":"subscribe","band":"alpha"} (again whenever it changes band) and
// receives one frame every 0.5 s plus events as they happen. With
// "links": true in the subscribe message it also gets a links message after
// each frame; "links": false stops them, and leaving the field out keeps
// the current setting. With "raw": [3, 10] it also gets the raw signal of
// those channels (at most 8) ten times a second; "raw": [] stops it.
func (a *api) liveSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !a.sessionExists(w, r, id) {
		return
	}
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: originPatterns(a.origins)})
	if err != nil {
		return // Accept has already answered the request
	}
	defer conn.CloseNow()

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	v := newViewer()
	a.hub.add(id, v)
	defer a.hub.remove(id, v)
	a.wsClients.Add(1)
	defer a.wsClients.Add(-1)

	// Reader: band and links changes. Any read error (the viewer left) ends the session.
	go func() {
		defer cancel()
		for {
			var msg struct {
				Type  string `json:"type"`
				Band  string `json:"band"`
				Links *bool  `json:"links"`
				Raw   *[]int `json:"raw"`
			}
			_, data, err := conn.Read(ctx)
			if err != nil {
				return
			}
			if json.Unmarshal(data, &msg) == nil && msg.Type == "subscribe" {
				if b, ok := bandIndex(strings.ToLower(msg.Band)); ok {
					v.band.Store(int32(b))
				}
				if msg.Links != nil {
					v.links.Store(*msg.Links)
				}
				if msg.Raw != nil {
					v.rawChannels.Store(rawSubscription(*msg.Raw))
				}
			}
		}
	}()

	write := func(msg any) error {
		data, err := json.Marshal(msg)
		if err != nil {
			return err
		}
		wctx, cancel := context.WithTimeout(ctx, wsWriteTimeout)
		defer cancel()
		return conn.Write(wctx, websocket.MessageText, data)
	}

	ping := time.NewTicker(wsPingEvery)
	defer ping.Stop()
	for {
		var err error
		select {
		case <-ctx.Done():
			conn.Close(websocket.StatusNormalClosure, "")
			return
		case f := <-v.frames:
			err = write(frameMessage(f, int(v.band.Load())))
			if err == nil && v.links.Load() {
				err = write(linksMessage(f))
			}
		case b := <-v.raw:
			// A batch queued just before the viewer unsubscribed is dropped here.
			if channels := v.rawChannels.Load(); channels != nil {
				err = write(rawMessage(b, *channels))
			}
		case e := <-v.events:
			details := json.RawMessage(e.DetailsJson)
			if !json.Valid(details) {
				details = json.RawMessage("{}")
			}
			err = write(wsEvent{Type: "event", EventType: e.Type, Channel: e.Channel,
				AtMs: e.TimeUnixNanos / int64(time.Millisecond), Details: details})
		case <-ping.C:
			pctx, cancel := context.WithTimeout(ctx, wsWriteTimeout)
			err = conn.Ping(pctx)
			cancel()
		}
		if err != nil {
			if !errors.Is(err, context.Canceled) {
				log.Printf("ws %s: %v", id, err)
			}
			return
		}
	}
}
