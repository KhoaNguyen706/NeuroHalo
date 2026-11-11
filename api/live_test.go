package main

import (
	"testing"

	neurov1 "github.com/neurohalo/neurohalo/gen/neuro/v1"
)

// TestSlowViewerKeepsNewestFrame checks that a viewer that is not reading
// holds only the newest frame and that the drop is counted.
func TestSlowViewerKeepsNewestFrame(t *testing.T) {
	h := newHub()
	v := newViewer()
	h.add("s1", v)
	for i := uint64(1); i <= 5; i++ {
		h.publishFrame(&neurov1.FeatureFrame{SessionId: "s1", WindowIndex: i})
	}
	if got := (<-v.frames).WindowIndex; got != 5 {
		t.Errorf("viewer got window %d, want the newest (5)", got)
	}
	if d := h.framesDropped.Load(); d != 4 {
		t.Errorf("dropped %d frames, want 4", d)
	}
	h.remove("s1", v)
	if len(h.viewers) != 0 {
		t.Error("session still listed after its last viewer left")
	}
}

// TestFrameMessage checks that one band is cut out per channel.
func TestFrameMessage(t *testing.T) {
	f := &neurov1.FeatureFrame{
		BandPower: []float32{.1, .2, .3, .4, .5, .6, .7, .8, .9, 1},
		Quality:   []neurov1.Quality{neurov1.Quality_QUALITY_OK, neurov1.Quality_QUALITY_FLAT},
	}
	msg := frameMessage(f, 2) // alpha
	if msg.Band != "alpha" || msg.Power[0] != .3 || msg.Power[1] != .8 || msg.Quality[1] != 2 {
		t.Errorf("got %+v", msg)
	}
}

// TestRawMessage checks that only the subscribed channels are cut out of a
// batch, in the subscribed order, and that an unknown channel gets no samples.
func TestRawMessage(t *testing.T) {
	b := &neurov1.SampleBatch{
		FirstSampleIndex: 32,
		NumSamples:       2,
		Values:           []float32{1, 2, 3, 10, 20, 30}, // 2 samples x 3 channels
	}
	msg := rawMessage(b, []int{2, 0, 7})
	if msg.FirstSample != 32 || len(msg.Samples) != 3 {
		t.Fatalf("got %+v", msg)
	}
	if msg.Samples[0][0] != 3 || msg.Samples[0][1] != 30 || msg.Samples[1][0] != 1 || msg.Samples[1][1] != 10 {
		t.Errorf("wrong samples: %v", msg.Samples)
	}
	if len(msg.Samples[2]) != 0 {
		t.Errorf("unknown channel got samples: %v", msg.Samples[2])
	}
}

// TestRawSubscription checks the limits on a raw subscription.
func TestRawSubscription(t *testing.T) {
	if rawSubscription(nil) != nil || rawSubscription([]int{-1}) != nil {
		t.Error("an empty or invalid list must turn raw off")
	}
	got := *rawSubscription([]int{4, 4, -2, 1, 2, 3, 5, 6, 7, 8, 9, 10})
	if len(got) != maxRawChannels || got[0] != 4 || got[1] != 1 {
		t.Errorf("got %v", got)
	}
}

// TestRawOnlyForSubscribers checks that raw batches reach only viewers that asked.
func TestRawOnlyForSubscribers(t *testing.T) {
	h := newHub()
	plain, detailed := newViewer(), newViewer()
	h.add("s1", plain)
	if h.wantsRaw("s1") {
		t.Error("nobody subscribed to raw yet")
	}
	h.add("s1", detailed)
	detailed.rawChannels.Store(rawSubscription([]int{0}))
	if !h.wantsRaw("s1") {
		t.Error("a viewer subscribed to raw")
	}
	h.publishRaw(&neurov1.SampleBatch{SessionId: "s1"})
	if len(detailed.raw) != 1 || len(plain.raw) != 0 {
		t.Errorf("queued %d for the subscriber and %d for the other", len(detailed.raw), len(plain.raw))
	}
}
