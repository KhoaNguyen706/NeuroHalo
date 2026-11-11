// loadgen opens many client streams to the gateway at once and replays the
// same recording on each at real speed, to measure how many sessions the
// pipeline holds. The latency numbers are read from Prometheus, not here.
//
//	uv run make_loadgen_data.py                                       (once, in replayer/)
//	docker compose run --rm loadgen -sessions 100 -duration 60s
package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"

	neurov1 "github.com/neurohalo/neurohalo/gen/neuro/v1"
)

const (
	batchEvery     = 100 * time.Millisecond
	streamsPerConn = 100 // sessions sharing one TCP connection
	statsEvery     = 5 * time.Second
)

// recording is the converted EDF file, held once and shared by every session.
// Built by: loadRecording. Used by: session.
type recording struct {
	SampleRateHz float64   `json:"sampleRateHz"`
	ChannelNames []string  `json:"channelNames"`
	Samples      int       `json:"samples"`
	values       []float32 // Samples x channels, row by row, microvolts
}

// counters are the totals over all sessions.
// Updated by: session. Read by: main (progress lines and the summary).
type counters struct {
	open    atomic.Int64  // streams open right now
	batches atomic.Uint64 // batches sent
	bytes   atomic.Uint64 // their encoded size
	late    atomic.Uint64 // batches sent more than 100 ms behind schedule
	failed  atomic.Uint64 // sessions that ended with an error
}

// loadRecording reads recording.json and recording.f32 from dir, the output
// of replayer/make_loadgen_data.py.
func loadRecording(dir string) (*recording, error) {
	var rec recording
	head, err := os.ReadFile(filepath.Join(dir, "recording.json"))
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(head, &rec); err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(filepath.Join(dir, "recording.f32"))
	if err != nil {
		return nil, err
	}
	if want := rec.Samples * len(rec.ChannelNames) * 4; len(raw) != want || want == 0 {
		return nil, fmt.Errorf("recording.f32 is %d bytes, the header says %d", len(raw), want)
	}
	rec.values = make([]float32, len(raw)/4)
	for i := range rec.values {
		rec.values[i] = math.Float32frombits(binary.LittleEndian.Uint32(raw[i*4:]))
	}
	return &rec, nil
}

// session runs one stream from start to finish: send the header, then one
// batch every 100 ms until ctx ends, then close the stream cleanly. Each send
// is scheduled against the start time, so timing does not drift. The
// recording starts over when it runs out; seq and the sample index keep counting.
func session(ctx context.Context, client neurov1.IngestClient, token, id string, rec *recording, c *counters) error {
	// The stream must outlive ctx by a moment, to close cleanly after the last batch.
	streamCtx, cancel := context.WithCancel(metadata.AppendToOutgoingContext(context.Background(), "authorization", "Bearer "+token))
	defer cancel()
	stream, err := client.Stream(streamCtx)
	if err != nil {
		return err
	}
	c.open.Add(1)
	defer c.open.Add(-1)

	err = stream.Send(&neurov1.IngestRequest{Msg: &neurov1.IngestRequest_Header{Header: &neurov1.SessionHeader{
		SessionId: id, SampleRateHz: rec.SampleRateHz, ChannelNames: rec.ChannelNames, Source: "loadgen",
	}}})
	if err != nil {
		return err
	}

	channels := len(rec.ChannelNames)
	perBatch := int(math.Round(rec.SampleRateHz * batchEvery.Seconds()))
	batchesInFile := rec.Samples / perBatch
	start := time.Now()
	for seq := uint64(0); ; seq++ {
		due := start.Add(time.Duration(seq) * batchEvery)
		select {
		case <-ctx.Done():
			_, err := stream.CloseAndRecv()
			return err
		case <-time.After(time.Until(due)):
		}
		if time.Since(due) > batchEvery {
			c.late.Add(1)
		}
		row := int(seq%uint64(batchesInFile)) * perBatch
		batch := &neurov1.SampleBatch{
			SessionId:         id,
			Seq:               seq,
			FirstSampleIndex:  seq * uint64(perBatch),
			SentTimeUnixNanos: time.Now().UnixNano(),
			NumSamples:        uint32(perBatch),
			Values:            rec.values[row*channels : (row+perBatch)*channels],
		}
		if err := stream.Send(&neurov1.IngestRequest{Msg: &neurov1.IngestRequest_Batch{Batch: batch}}); err != nil {
			// Send only says the stream broke; CloseAndRecv gives the gateway's reason.
			if _, closeErr := stream.CloseAndRecv(); closeErr != nil {
				return closeErr
			}
			return err
		}
		c.batches.Add(1)
		c.bytes.Add(uint64(proto.Size(batch)))
	}
}

// main reads the flags, opens the connections, starts the sessions spread
// over the ramp time, prints a progress line every 5 seconds and a summary
// at the end. Ctrl+C ends the run early and still closes the streams cleanly.
func main() {
	gateway := flag.String("gateway", "gateway:50051", "gateway address")
	sessions := flag.Int("sessions", 10, "streams to hold open")
	duration := flag.Duration("duration", time.Minute, "how long to send, after the ramp")
	ramp := flag.Duration("ramp", 10*time.Second, "time over which the streams are started")
	data := flag.String("data", "/data", "folder with recording.f32 and recording.json")
	flag.Parse()
	token := os.Getenv("INGEST_TOKEN")
	if token == "" {
		log.Fatal("INGEST_TOKEN is not set")
	}

	rec, err := loadRecording(*data)
	if err != nil {
		log.Fatalf("recording: %v (run replayer/make_loadgen_data.py first)", err)
	}

	var clients []neurov1.IngestClient
	for i := 0; i < (*sessions+streamsPerConn-1)/streamsPerConn; i++ {
		conn, err := grpc.NewClient(*gateway, grpc.WithTransportCredentials(insecure.NewCredentials()))
		if err != nil {
			log.Fatalf("gateway: %v", err)
		}
		defer conn.Close()
		clients = append(clients, neurov1.NewIngestClient(conn))
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, *ramp+*duration)
	defer cancel()

	run := fmt.Sprintf("%x", time.Now().Unix()) // keeps this run's session IDs apart from earlier runs
	log.Printf("run %s: %d sessions, %d channels at %g Hz, ramp %v, then %v", run, *sessions, len(rec.ChannelNames), rec.SampleRateHz, *ramp, *duration)

	var c counters
	var wg sync.WaitGroup
	for i := 0; i < *sessions; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case <-ctx.Done():
				return
			case <-time.After(*ramp * time.Duration(i) / time.Duration(*sessions)):
			}
			id := fmt.Sprintf("load-%s-%04d", run, i)
			if err := session(ctx, clients[i%len(clients)], token, id, rec, &c); err != nil {
				if c.failed.Add(1) <= 5 { // the first few say why; the rest are only counted
					log.Printf("session %s: %v", id, err)
				}
			}
		}()
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	began := time.Now()
	tick := time.NewTicker(statsEvery)
	defer tick.Stop()
	var lastBatches, lastBytes uint64
	for running := true; running; {
		select {
		case <-done:
			running = false
		case <-tick.C:
			b, by := c.batches.Load(), c.bytes.Load()
			log.Printf("open=%d  %.0f batches/s  %.2f MB/s  late=%d  failed=%d", c.open.Load(),
				float64(b-lastBatches)/statsEvery.Seconds(), float64(by-lastBytes)/statsEvery.Seconds()/1e6,
				c.late.Load(), c.failed.Load())
			lastBatches, lastBytes = b, by
		}
	}
	seconds := time.Since(began).Seconds()
	log.Printf("done in %.0f s: %d batches (%.0f/s average), %.1f MB, %d late, %d of %d sessions failed",
		seconds, c.batches.Load(), float64(c.batches.Load())/seconds, float64(c.bytes.Load())/1e6,
		c.late.Load(), c.failed.Load(), *sessions)
}
