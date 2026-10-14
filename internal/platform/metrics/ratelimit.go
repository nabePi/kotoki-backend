package metrics

import "github.com/prometheus/client_golang/prometheus"

// RateLimit counts rate-limiter outcomes. Policy names come from the fixed catalog,
// so label cardinality stays bounded.
type RateLimit struct {
	rejected      *prometheus.CounterVec
	backendErrors prometheus.Counter
}

// NewRateLimit registers the rate-limit metrics on reg.
func NewRateLimit(reg prometheus.Registerer) *RateLimit {
	m := &RateLimit{
		rejected: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kotoki_ratelimit_rejected_total",
			Help: "Requests rejected by the rate limiter, by policy.",
		}, []string{"policy"}),
		backendErrors: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "kotoki_ratelimit_backend_errors_total",
			Help: "Rate limiter backend (Redis) errors; requests fall back to the local limiter.",
		}),
	}
	reg.MustRegister(m.rejected, m.backendErrors)
	return m
}

// Rejected counts one rejected request for policy.
func (m *RateLimit) Rejected(policy string) { m.rejected.WithLabelValues(policy).Inc() }

// BackendError counts one limiter backend failure.
func (m *RateLimit) BackendError() { m.backendErrors.Inc() }
