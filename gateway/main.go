// The gateway checks the token, validates batches and forwards them.
// It never computes anything.
package main

import (
	"context"
	"crypto/subtle"
	"errors"
	"io"
	"log"
	"math"
	"net"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/twmb/franz-go/pkg/kgo"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/neurohalo/neurohalo/internal/metrics"

	neurov1 "github.com/neurohalo/neurohalo/gen/neuro/v1"
)

// The gateway's Prometheus metrics.
var (
	// ingestBatches counts batches Kafka has confirmed. Raised by: server.publish.
	ingestBatches = promauto.NewCounter(prometheus.CounterOpts{
		Name: "ingest_batches_total",
		Help: "Batches accepted and stored in Kafka.",
	})
	// ingestBytes counts the encoded size of those batches. Raised by: server.publish.
	ingestBytes = promauto.NewCounter(prometheus.CounterOpts{
		Name: "ingest_bytes_total",
		Help: "Encoded bytes of the batches stored in Kafka.",
	})
	// activeStreams is how many client streams are open right now. Moved by: server.Stream.
	activeStreams = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "active_streams",
		Help: "Client streams open right now.",
	})
	// rejectedBatches counts batches refused, by reason: session_id,
	// num_samples, values (wrong count) or not_batch. Raised by: validateBatch, server.Stream.
	rejectedBatches = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "rejected_batches_total",
		Help: "Batches refused by validation, by reason.",
	}, []string{"reason"})
)

const (
	rawTopic       = "eeg.raw"
	batchSeconds   = 0.1 // a batch covers 100 ms
	logEvery       = 5 * time.Second
	publishTimeout = 5 * time.Second
	dbTimeout      = 5 * time.Second
)

// server implements the Ingest gRPC service and holds what its handlers need.
// Created by: main.
// Used by: authorized, saveSession, endSession, publish, Stream (its methods).
type server struct {
	neurov1.UnimplementedIngestServer
	token string
	kafka *kgo.Client
	db    *pgxpool.Pool
}

// authorized reports whether the stream's metadata carries
// "authorization: Bearer <token>" with the gateway's token.
func (s *server) authorized(stream grpc.ServerStream) bool {
	md, _ := metadata.FromIncomingContext(stream.Context())
	for _, v := range md.Get("authorization") {
		got, ok := strings.CutPrefix(v, "Bearer ")
		if ok && subtle.ConstantTimeCompare([]byte(got), []byte(s.token)) == 1 {
			return true
		}
	}
	return false
}

// validateHeader checks the first message of a stream: it must be a header
// with a session ID, a positive sample rate and at least one channel.
// It returns an INVALID_ARGUMENT error otherwise.
func validateHeader(h *neurov1.SessionHeader) error {
	switch {
	case h == nil:
		return status.Error(codes.InvalidArgument, "first message must be a header")
	case h.SessionId == "":
		return status.Error(codes.InvalidArgument, "header: session_id is empty")
	case !(h.SampleRateHz > 0) || math.IsInf(h.SampleRateHz, 0):
		return status.Error(codes.InvalidArgument, "header: sample_rate_hz must be positive")
	case len(h.ChannelNames) == 0:
		return status.Error(codes.InvalidArgument, "header: channel_names is empty")
	}
	return nil
}

// validateBatch checks one batch against its session's header: same session
// ID, at most 100 ms of samples, and exactly num_samples x channels values.
// It returns an INVALID_ARGUMENT error otherwise.
func validateBatch(h *neurov1.SessionHeader, b *neurov1.SampleBatch) error {
	maxSamples := uint32(math.Ceil(h.SampleRateHz * batchSeconds))
	want := int(b.NumSamples) * len(h.ChannelNames)
	switch {
	case b.SessionId != h.SessionId:
		rejectedBatches.WithLabelValues("session_id").Inc()
		return status.Errorf(codes.InvalidArgument, "batch %d: session_id %q does not match header %q", b.Seq, b.SessionId, h.SessionId)
	case b.NumSamples == 0 || b.NumSamples > maxSamples:
		rejectedBatches.WithLabelValues("num_samples").Inc()
		return status.Errorf(codes.InvalidArgument, "batch %d: num_samples %d outside 1..%d", b.Seq, b.NumSamples, maxSamples)
	case len(b.Values) != want:
		rejectedBatches.WithLabelValues("values").Inc()
		return status.Errorf(codes.InvalidArgument, "batch %d: %d values, want %d (num_samples x channels)", b.Seq, len(b.Values), want)
	}
	return nil
}

// saveSession stores the session details so they do not travel in every Kafka
// message. A reconnect resends the same header, which puts the session back to live.
func (s *server) saveSession(ctx context.Context, h *neurov1.SessionHeader) error {
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	_, err := s.db.Exec(ctx, `
		INSERT INTO sessions (id, source, sample_rate_hz, channel_names, started_at)
		VALUES ($1, $2, $3, $4, now())
		ON CONFLICT (id) DO UPDATE SET status = 'live', ended_at = NULL`,
		h.SessionId, h.Source, h.SampleRateHz, h.ChannelNames)
	return err
}

// endSession marks a session as ended in Postgres after its stream closes
// cleanly. A failure is only logged, because the data is already in Kafka.
func (s *server) endSession(id string) {
	// The stream's own context is already finished here, so use a fresh one.
	ctx, cancel := context.WithTimeout(context.Background(), dbTimeout)
	defer cancel()
	if _, err := s.db.Exec(ctx,
		`UPDATE sessions SET status = 'ended', ended_at = now() WHERE id = $1`, id); err != nil {
		log.Printf("session %s: could not mark ended: %v", id, err)
	}
}

// publish waits for Kafka to confirm the batch, so a batch the client is told
// about is a batch that is stored. Batches of one session go out one at a time,
// which keeps them in order.
func (s *server) publish(ctx context.Context, b *neurov1.SampleBatch) error {
	value, err := proto.Marshal(b)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, publishTimeout)
	defer cancel()
	rec := &kgo.Record{Topic: rawTopic, Key: []byte(b.SessionId), Value: value}
	if err := s.kafka.ProduceSync(ctx, rec).FirstErr(); err != nil {
		return err
	}
	ingestBatches.Inc()
	ingestBytes.Add(float64(len(value)))
	return nil
}

// Stream handles one client stream from start to finish: check the token,
// read and save the header, then validate and publish each batch until the
// client closes the stream, and answer with how many batches were stored.
func (s *server) Stream(stream grpc.ClientStreamingServer[neurov1.IngestRequest, neurov1.IngestSummary]) error {
	if !s.authorized(stream) {
		return status.Error(codes.Unauthenticated, "missing or wrong bearer token")
	}
	ctx := stream.Context()
	activeStreams.Inc()
	defer activeStreams.Dec()

	first, err := stream.Recv()
	if err != nil {
		return err
	}
	header := first.GetHeader()
	if err := validateHeader(header); err != nil {
		return err
	}
	if err := s.saveSession(ctx, header); err != nil {
		log.Printf("session %s: could not save session: %v", header.SessionId, err)
		return status.Error(codes.Unavailable, "session store is unreachable")
	}
	log.Printf("session %s: stream opened, %d channels at %g Hz, source %q",
		header.SessionId, len(header.ChannelNames), header.SampleRateHz, header.Source)

	var summary neurov1.IngestSummary
	lastLog := time.Now()
	for {
		req, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			s.endSession(header.SessionId)
			log.Printf("session %s: stream closed, %d batches, last seq %d",
				header.SessionId, summary.BatchesReceived, summary.LastSeq)
			return stream.SendAndClose(&summary)
		}
		if err != nil {
			log.Printf("session %s: stream broke after %d batches: %v",
				header.SessionId, summary.BatchesReceived, err)
			return err
		}
		batch := req.GetBatch()
		if batch == nil {
			rejectedBatches.WithLabelValues("not_batch").Inc()
			return status.Error(codes.InvalidArgument, "only the first message may be a header")
		}
		if err := validateBatch(header, batch); err != nil {
			log.Printf("session %s: rejected: %v", header.SessionId, err)
			return err
		}
		if err := s.publish(ctx, batch); err != nil {
			log.Printf("session %s: publish seq %d failed: %v", header.SessionId, batch.Seq, err)
			return status.Error(codes.Unavailable, "event stream is unreachable")
		}

		summary.BatchesReceived++
		summary.LastSeq = batch.Seq
		if time.Since(lastLog) >= logEvery {
			log.Printf("session %s: %d batches published, last seq %d",
				header.SessionId, summary.BatchesReceived, summary.LastSeq)
			lastLog = time.Now()
		}
	}
}

// env returns an environment variable, or fallback when it is unset or empty.
func env(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

// main reads the configuration, connects to Kafka and Postgres, serves
// Prometheus metrics on METRICS_ADDR, and serves the Ingest service over gRPC
// until the process is stopped.
func main() {
	token := os.Getenv("INGEST_TOKEN")
	if token == "" {
		log.Fatal("INGEST_TOKEN is not set")
	}
	addr := env("LISTEN_ADDR", ":50051")
	metrics.Serve(env("METRICS_ADDR", ":9100"))

	// franz-go defaults already match the design: acks=all with idempotence on.
	kafka, err := kgo.NewClient(
		kgo.SeedBrokers(strings.Split(env("KAFKA_BROKERS", "localhost:29092"), ",")...),
		kgo.RecordDeliveryTimeout(publishTimeout),
	)
	if err != nil {
		log.Fatalf("kafka: %v", err)
	}
	defer kafka.Close()

	db, err := pgxpool.New(context.Background(), env("DATABASE_URL", "postgres://neuro:neuro@localhost:5432/neuro"))
	if err != nil {
		log.Fatalf("postgres: %v", err)
	}
	defer db.Close()

	lis, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatalf("listen: %v", err)
	}
	srv := grpc.NewServer()
	neurov1.RegisterIngestServer(srv, &server{token: token, kafka: kafka, db: db})
	log.Printf("gateway listening on %s", addr)
	if err := srv.Serve(lis); err != nil {
		log.Fatalf("serve: %v", err)
	}
}
