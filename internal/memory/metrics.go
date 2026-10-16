package memory

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Metrics are the memory collectors (spec 13.7); this file holds the extraction ones, later tasks
// add the rest. Labels never carry user text or ids. All methods are safe on a nil *Metrics.
type Metrics struct {
	candidates *prometheus.CounterVec
	rejected   *prometheus.CounterVec
	applied    *prometheus.CounterVec

	extraction *prometheus.HistogramVec
	retrieval  *prometheus.HistogramVec
	degraded   *prometheus.CounterVec
	ctxTokens  *prometheus.HistogramVec
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
		extraction: f.NewHistogramVec(prometheus.HistogramOpts{
			Name: "kotoki_memory_extraction_duration_seconds", Help: "Duration of one memory extraction delivery, by result.",
			Buckets: []float64{0.1, 0.5, 1, 2, 5, 10, 20, 40, 80, 120}}, []string{"result"}),
		retrieval: f.NewHistogramVec(prometheus.HistogramOpts{
			Name: "kotoki_memory_retrieval_duration_seconds", Help: "Duration of a memory retrieval, by stage (start|turn).",
			Buckets: []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 1.5, 2}}, []string{"stage"}),
		degraded: f.NewCounterVec(prometheus.CounterOpts{
			Name: "kotoki_memory_retrieval_degraded_total", Help: "Memory retrievals that degraded, by stage."}, []string{"stage"}),
		ctxTokens: f.NewHistogramVec(prometheus.HistogramOpts{
			Name: "kotoki_memory_context_tokens", Help: "Estimated tokens of the memory block, by part (core|episodic).",
			Buckets: []float64{0, 50, 100, 200, 300, 400, 500, 600}}, []string{"part"}),
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

// Extraction records the duration of one extraction delivery; result is "ok", "skipped" or "error".
func (m *Metrics) Extraction(result string, d time.Duration) {
	if m != nil {
		m.extraction.WithLabelValues(result).Observe(d.Seconds())
	}
}

// Retrieval records the duration of one retrieval of stage "start" or "turn".
func (m *Metrics) Retrieval(stage string, d time.Duration) {
	if m != nil {
		m.retrieval.WithLabelValues(stage).Observe(d.Seconds())
	}
}

// Degraded counts one retrieval of stage that returned a degraded result.
func (m *Metrics) Degraded(stage string) {
	if m != nil {
		m.degraded.WithLabelValues(stage).Inc()
	}
}

// ContextTokens records the tokens of part "core" or "episodic" of a built memory block.
func (m *Metrics) ContextTokens(part string, tokens int) {
	if m != nil {
		m.ctxTokens.WithLabelValues(part).Observe(float64(tokens))
	}
}
