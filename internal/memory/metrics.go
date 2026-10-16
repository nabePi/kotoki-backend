package memory

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Metrics are the memory collectors (spec 13.7); this file holds the extraction ones, later tasks
// add the rest. Labels never carry user text or ids. All methods are safe on a nil *Metrics.
type Metrics struct {
	candidates *prometheus.CounterVec
	rejected   *prometheus.CounterVec
	applied    *prometheus.CounterVec
}

// NewMetrics registers the memory collectors on reg.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	f := promauto.With(reg)
	return &Metrics{
		candidates: f.NewCounterVec(prometheus.CounterOpts{
			Name: "kotoki_memory_candidates_total", Help: "Memory candidates that passed validation, by category."}, []string{"category"}),
		rejected: f.NewCounterVec(prometheus.CounterOpts{
			Name: "kotoki_memory_candidates_rejected_total", Help: "Memory candidates rejected by validation, by reason."}, []string{"reason"}),
		applied: f.NewCounterVec(prometheus.CounterOpts{
			Name: "kotoki_memory_apply_total", Help: "Memory candidates applied, by action."}, []string{"action"}),
	}
}

// Candidate counts one candidate that passed validation.
func (m *Metrics) Candidate(c Category) {
	if m != nil {
		m.candidates.WithLabelValues(string(c)).Inc()
	}
}

// Rejected counts one rejected candidate.
func (m *Metrics) Rejected(reason string) {
	if m != nil {
		m.rejected.WithLabelValues(reason).Inc()
	}
}

// Applied counts one ApplyCandidate outcome.
func (m *Metrics) Applied(a Action) {
	if m != nil {
		m.applied.WithLabelValues(string(a)).Inc()
	}
}
