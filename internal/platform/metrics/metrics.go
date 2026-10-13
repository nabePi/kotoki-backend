// Package metrics provides the Prometheus registry, shared histogram buckets
// and client-side metrics for infrastructure dependencies. Label values must
// stay low-cardinality: never user_id, session_id, turn_id or email.
package metrics

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Histogram bucket boundaries in seconds.
var (
	HTTPBuckets  = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.2, 0.3, 0.5, 1, 2.5, 5}
	TurnBuckets  = []float64{0.1, 0.2, 0.3, 0.4, 0.5, 0.6, 0.8, 1, 1.25, 1.5, 2, 3, 5, 10}
	BargeBuckets = []float64{0.01, 0.025, 0.05, 0.075, 0.1, 0.15, 0.2, 0.3, 0.5, 0.6, 1}
	AsyncBuckets = []float64{0.1, 0.5, 1, 2, 5, 10, 20, 30, 60, 120, 300, 600}
)

// acquireWaitBuckets are the buckets for pool acquire wait time.
var acquireWaitBuckets = []float64{0.001, 0.005, 0.01, 0.05, 0.1, 0.5, 1}

// NewRegistry returns a new registry with Go runtime and process collectors.
// It does not use the global default registry.
func NewRegistry() *prometheus.Registry {
	reg := prometheus.NewRegistry()
	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	return reg
}

// Handler serves reg in the Prometheus exposition format.
func Handler(reg *prometheus.Registry) http.Handler {
	return promhttp.HandlerFor(reg, promhttp.HandlerOpts{})
}
