// featuretail prints what the processor writes, for checking the pipeline in a
// terminal.
//
//	docker compose run --rm featuretail            # follow live frames and events
//	docker compose run --rm featuretail -summary   # read everything, print averages per session
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/twmb/franz-go/pkg/kgo"
	"google.golang.org/protobuf/proto"

	"github.com/neurohalo/neurohalo/internal/eeg"

	neurov1 "github.com/neurohalo/neurohalo/gen/neuro/v1"
)

// sessionInfo is what featuretail knows and has counted about one session.
// Used by: main (the lookup helper creates it, the loop fills it, the summary prints it).
type sessionInfo struct {
	names []string
	back  []int

	frames              int
	backAlpha, allAlpha float64 // sums over frames
	events              map[string]int
}

// env returns an environment variable, or fallback when it is unset or empty.
func env(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

// main reads eeg.features and eeg.events and prints one line per frame and
// event, or with -summary reads everything and prints averages per session.
func main() {
	summary := flag.Bool("summary", false, "read from the start until idle, then print averages per session")
	only := flag.String("session", "", "show only this session")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	offset := kgo.NewOffset().AtEnd()
	if *summary {
		offset = kgo.NewOffset().AtStart()
	}
	kafka, err := kgo.NewClient(
		kgo.SeedBrokers(strings.Split(env("KAFKA_BROKERS", "localhost:29092"), ",")...),
		kgo.ConsumeTopics("eeg.features", "eeg.events"),
		kgo.ConsumeResetOffset(offset),
	)
	if err != nil {
		log.Fatalf("kafka: %v", err)
	}
	defer kafka.Close()
	db, err := pgxpool.New(ctx, env("DATABASE_URL", "postgres://neuro:neuro@localhost:5432/neuro"))
	if err != nil {
		log.Fatalf("postgres: %v", err)
	}
	defer db.Close()

	sessions := map[string]*sessionInfo{}
	lookup := func(id string) *sessionInfo {
		if s, ok := sessions[id]; ok {
			return s
		}
		s := &sessionInfo{events: map[string]int{}}
		if err := db.QueryRow(ctx, `SELECT channel_names FROM sessions WHERE id = $1`, id).Scan(&s.names); err != nil {
			log.Printf("session %s: no channel names: %v", id, err)
		}
		for i, name := range s.names {
			if eeg.BackOfHead(name) {
				s.back = append(s.back, i)
			}
		}
		sessions[id] = s
		return s
	}

	for {
		pollCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		fetches := kafka.PollFetches(pollCtx)
		idle := pollCtx.Err() != nil && ctx.Err() == nil
		cancel()
		if ctx.Err() != nil || (idle && *summary) {
			break
		}
		fetches.EachRecord(func(rec *kgo.Record) {
			id := string(rec.Key)
			if *only != "" && id != *only {
				return
			}
			s := lookup(id)
			if rec.Topic == "eeg.events" {
				var e neurov1.Event
				if proto.Unmarshal(rec.Value, &e) != nil {
					return
				}
				s.events[e.Type]++
				if !*summary {
					fmt.Printf("%-18s EVENT %s %s %s\n", id, e.Type, e.Channel, e.DetailsJson)
				}
				return
			}
			var f neurov1.FeatureFrame
			if proto.Unmarshal(rec.Value, &f) != nil || len(f.Quality) != len(s.names) {
				return
			}
			alpha := func(c int) float64 { return float64(f.BandPower[c*eeg.NumBands+eeg.AlphaIndex]) }
			var back, all float64
			for _, c := range s.back {
				back += alpha(c) / float64(len(s.back))
			}
			var flat, noisy int
			for c, q := range f.Quality {
				all += alpha(c) / float64(len(f.Quality))
				switch q {
				case neurov1.Quality_QUALITY_FLAT:
					flat++
				case neurov1.Quality_QUALITY_NOISY:
					noisy++
				}
			}
			s.frames++
			s.backAlpha += back
			s.allAlpha += all
			if !*summary {
				lag := time.Since(time.Unix(0, f.NewestSentTimeUnixNanos)).Milliseconds()
				fmt.Printf("%-18s t=%5.1fs back-alpha=%.3f all-alpha=%.3f flat=%d noisy=%d lag=%dms\n",
					id, float64(f.WindowIndex)*0.5, back, all, flat, noisy, lag)
			}
		})
	}

	ids := make([]string, 0, len(sessions))
	for id := range sessions {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	fmt.Printf("\n%-20s %7s %11s %10s  %s\n", "session", "frames", "back-alpha", "all-alpha", "events")
	for _, id := range ids {
		s := sessions[id]
		if s.frames == 0 {
			continue
		}
		n := float64(s.frames)
		fmt.Printf("%-20s %7d %11.3f %10.3f  %v\n", id, s.frames, s.backAlpha/n, s.allAlpha/n, s.events)
	}
}
