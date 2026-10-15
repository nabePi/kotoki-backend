package conversation

import "github.com/prometheus/client_golang/prometheus"

// Metrics holds the realtime metrics (spec §7.14, prefix kotoki_realtime_). This is the subset
// the connection plumbing (reader, writer, Outbound) and the generation pipeline need; Task 18
// extends the same type with the rest of §7.14. Labels never carry session_id or user_id. Every
// method is safe on a nil *Metrics, so tests and partial wiring may pass nil.
type Metrics struct {
	audioFramesIn      *prometheus.CounterVec
	audioFramesOut     *prometheus.CounterVec
	protocolViolations *prometheus.CounterVec
	slowConsumer       prometheus.Counter
	outboundDropped    *prometheus.CounterVec
	aiSentenceLimit    *prometheus.CounterVec
	llmTruncated       prometheus.Counter
	difficultyAdjusted prometheus.Counter
}

// NewMetrics registers the metrics on reg (nil → a private registry, for tests).
func NewMetrics(reg prometheus.Registerer) *Metrics {
	if reg == nil {
		reg = prometheus.NewRegistry()
	}
	m := &Metrics{
		audioFramesIn: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "kotoki_realtime_audio_frames_in_total",
			Help: "Upstream audio frames by result."}, []string{"result"}),
		audioFramesOut: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "kotoki_realtime_audio_frames_out_total",
			Help: "Downstream audio frames by result."}, []string{"result"}),
		protocolViolations: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "kotoki_realtime_protocol_violations_total",
			Help: "Connections closed 1008 by violation kind."}, []string{"kind"}),
		slowConsumer: prometheus.NewCounter(prometheus.CounterOpts{Name: "kotoki_realtime_slow_consumer_total",
			Help: "Connections dropped because the outbound queue was full."}),
		outboundDropped: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "kotoki_realtime_outbound_transient_dropped_total",
			Help: "Transient control messages replaced by a newer one (coalesced) or dropped."}, []string{"reason"}),
		aiSentenceLimit: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "kotoki_realtime_ai_sentence_limit_total",
			Help: "Generations stopped by the R-17 sentence limit."}, []string{"kind"}),
		llmTruncated: prometheus.NewCounter(prometheus.CounterOpts{Name: "kotoki_realtime_llm_truncated_total",
			Help: "LLM streams the router ended with DONE{TRUNCATED} after audio was sent (S-7)."}),
		difficultyAdjusted: prometheus.NewCounter(prometheus.CounterOpts{Name: "kotoki_realtime_difficulty_adjustments_total",
			Help: "In-session adaptive difficulty adjustments (spec 7.8.6)."}),
	}
	reg.MustRegister(m.audioFramesIn, m.audioFramesOut, m.protocolViolations, m.slowConsumer, m.outboundDropped,
		m.aiSentenceLimit, m.llmTruncated, m.difficultyAdjusted)
	return m
}

func (m *Metrics) audioIn(result string) {
	if m != nil {
		m.audioFramesIn.WithLabelValues(result).Inc()
	}
}

func (m *Metrics) audioOut(result string, n int) {
	if m != nil && n > 0 {
		m.audioFramesOut.WithLabelValues(result).Add(float64(n))
	}
}

func (m *Metrics) protocolViolation(kind string) {
	if m != nil {
		m.protocolViolations.WithLabelValues(kind).Inc()
	}
}

func (m *Metrics) slowConsumerInc() {
	if m != nil {
		m.slowConsumer.Inc()
	}
}

func (m *Metrics) transientDropped(reason string) {
	if m != nil {
		m.outboundDropped.WithLabelValues(reason).Inc()
	}
}

func (m *Metrics) sentenceLimit(kind string) {
	if m != nil {
		m.aiSentenceLimit.WithLabelValues(kind).Inc()
	}
}

func (m *Metrics) llmTruncatedInc() {
	if m != nil {
		m.llmTruncated.Inc()
	}
}

func (m *Metrics) difficultyAdjustedInc() {
	if m != nil {
		m.difficultyAdjusted.Inc()
	}
}
