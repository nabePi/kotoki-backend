package metrics

import (
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

var httpMethods = map[string]bool{"GET": true, "POST": true, "PUT": true, "PATCH": true, "DELETE": true, "OPTIONS": true, "HEAD": true}

// HTTP records request counts and latency per route pattern. Labels never carry
// user_id, email or raw paths containing identifiers.
type HTTP struct {
	service  string
	requests *prometheus.CounterVec
	duration *prometheus.HistogramVec
}

// NewHTTP registers the HTTP server metrics for service on reg.
func NewHTTP(reg prometheus.Registerer, service string) *HTTP {
	h := &HTTP{
		service: service,
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kotoki_http_requests_total",
			Help: "HTTP requests by service, method, route pattern and status.",
		}, []string{"service", "method", "route", "status"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "kotoki_http_request_duration_seconds",
			Help:    "HTTP request duration in seconds.",
			Buckets: HTTPBuckets,
		}, []string{"service", "method", "route"}),
	}
	reg.MustRegister(h.requests, h.duration)
	return h
}

// Observe records one finished request; route must be a pattern or "unmatched".
func (h *HTTP) Observe(method, route string, status int, d time.Duration) {
	if !httpMethods[method] {
		method = "OTHER"
	}
	h.requests.WithLabelValues(h.service, method, route, strconv.Itoa(status)).Inc()
	h.duration.WithLabelValues(h.service, method, route).Observe(d.Seconds())
}
