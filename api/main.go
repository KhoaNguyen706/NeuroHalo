// The api service serves session history over REST and stores events and
// rollups. The live WebSocket arrives in build step 6.
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
)

// env returns an environment variable, or fallback when it is unset or empty.
func env(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

// main connects to Postgres and Kafka, starts the store consumer and the HTTP
// server, and runs until Ctrl+C or SIGTERM. If the store consumer fails, the
// process exits so the container restarts and resumes from the last commit.
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

	srv := &http.Server{
		Addr:              env("LISTEN_ADDR", ":8080"),
		Handler:           (&api{db: db, token: token}).routes(),
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
