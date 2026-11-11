// Package metrics serves a program's Prometheus metrics over HTTP. The
// metrics themselves are declared next to the code that updates them.
package metrics

import (
	"log"
	"net/http"

	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Serve answers GET /metrics on addr (":9100") in the background. The port is
// separate from the service's own, so it needs no token and is never
// published outside the Compose network.
// Used by: the main functions of gateway, processor and api.
func Serve(addr string) {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	go func() {
		if err := http.ListenAndServe(addr, mux); err != nil {
			log.Printf("metrics: %v", err)
		}
	}()
}
