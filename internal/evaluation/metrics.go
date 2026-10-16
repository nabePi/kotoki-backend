package evaluation

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"github.com/nabePi/kotoki-backend/internal/platform/metrics"
)

// Metrics are the evaluation collectors (spec 13.7). Labels never carry user text or ids. All
// methods are safe on a nil *Metrics.
type Metrics struct {
	llmCalls        *prometheus.CounterVec
	findingsDropped *prometheus.CounterVec
	latency         prometheus.Histogram
}

// NewMetrics registers the collectors on reg.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	f := promauto.With(reg)
	return &Metrics{
		llmCalls: f.NewCounterVec(prometheus.CounterOpts{
			Name: "kotoki_evaluation_llm_calls_total", Help: "Evaluation LLM calls by provider and result."}, []string{"provider", "result"}),
		findingsDropped: f.NewCounterVec(prometheus.CounterOpts{
			Name: "kotoki_evaluation_findings_dropped_total", Help: "Findings dropped by the validator, by reason."}, []string{"reason"}),
		latency: f.NewHistogram(prometheus.HistogramOpts{
			Name: "kotoki_evaluation_latency_seconds", Help: "Evaluation time per delivery.", Buckets: metrics.AsyncBuckets}),
	}
}

// LLMCall counts one LLM call; result is valid|invalid|error.
func (m *Metrics) LLMCall(provider, result string) {
	if m != nil {
		m.llmCalls.WithLabelValues(provider, result).Inc()
	}
}

// FindingDropped counts one dropped finding.
func (m *Metrics) FindingDropped(reason string) {
	if m != nil {
		m.findingsDropped.WithLabelValues(reason).Inc()
	}
}

// ObserveLatency records the evaluation duration in seconds.
func (m *Metrics) ObserveLatency(seconds float64) {
	if m != nil {
		m.latency.Observe(seconds)
	}
}
