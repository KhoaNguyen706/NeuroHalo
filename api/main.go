// The api service serves session history over REST, pushes live frames over
// WebSocket, stores events and rollups, and accepts uploads.
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/neurohalo/neurohalo/internal/metrics"
)

// env returns an environment variable, or fallback when it is unset or empty.
func env(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

// main connects to Postgres, Kafka and the object store, starts the store
// consumer, the live consumer, the HTTP server and Prometheus metrics on
// METRICS_ADDR, and runs until Ctrl+C or
// SIGTERM. If the store consumer fails, the process exits so the container
// restarts and resumes from the last commit.
func main() {
	token := os.Getenv("API_TOKEN")
	if token == "" {
		log.Fatal("API_TOKEN is not set")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	db, err := pgxpool.New(ctx, env("DATABASE_URL", "postgres://neuro:neuro@localhost:5432/neuro"))
	if err != nil {
		log.Fatalf("postgres: %v", err)
	}
	defer db.Close()

	st := newStore(db)
	st.kafka, err = kgo.NewClient(
		kgo.SeedBrokers(strings.Split(env("KAFKA_BROKERS", "localhost:29092"), ",")...),
		kgo.ClientID("api-store"),
		kgo.ConsumerGroup(storeGroup),
		kgo.ConsumeTopics(featuresTopic, eventsTopic),
		kgo.DisableAutoCommit(),
		kgo.BlockRebalanceOnPoll(), // rebalance callbacks never run in the middle of a pass
		kgo.OnPartitionsRevoked(st.dropPartitions),
		kgo.OnPartitionsLost(st.dropPartitions),
	)
	if err != nil {
		log.Fatalf("kafka: %v", err)
	}
	defer st.kafka.Close()

	brokers := strings.Split(env("KAFKA_BROKERS", "localhost:29092"), ",")
	a := &api{
		db:      db,
		token:   token,
		hub:     newHub(),
		origins: strings.Split(env("WEB_ORIGIN", "http://localhost:3000"), ","),
	}
	registerLiveMetrics(a)
	metrics.Serve(env("METRICS_ADDR", ":9100"))

	if endpoint := os.Getenv("S3_ENDPOINT"); endpoint != "" {
		a.objects, err = newObjectStore(endpoint, os.Getenv("S3_ACCESS_KEY"), os.Getenv("S3_SECRET_KEY"),
			env("S3_BUCKET", "recordings"), os.Getenv("S3_SECURE") == "true")
		if err != nil {
			log.Fatalf("object store: %v", err)
		}
		go a.objects.ensureBucket(ctx)
	} else {
		log.Print("S3_ENDPOINT is not set, uploads are off")
	}

	go func() {
		if err := runLiveConsumer(ctx, brokers, a.hub); err != nil {
			log.Fatalf("live consumer stopped: %v", err)
		}
	}()

	srv := &http.Server{
		Addr:              env("LISTEN_ADDR", ":8080"),
		Handler:           a.routes(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		log.Printf("api listening on %s", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("http: %v", err)
		}
	}()

	log.Printf("store consumer started, group %q reading %s and %s", storeGroup, featuresTopic, eventsTopic)
	if err := st.run(ctx); err != nil {
		log.Fatalf("store stopped: %v", err)
	}
	shutdown(srv)
	log.Print("api stopped")
}
