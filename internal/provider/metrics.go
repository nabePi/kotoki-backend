package provider

import (
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

var latencyBuckets = []float64{0.05, 0.1, 0.2, 0.3, 0.4, 0.5, 0.75, 1, 1.5, 2, 3, 5, 10, 20, 60}

// knownVendorCodes bounds the cardinality of the vendor_code label; anything else is "other".
var knownVendorCodes = map[string]bool{
	"insufficient_quota": true, "rate_limit_exceeded": true, "local_concurrency": true, "circuit_open": true,
	"voice_binding_missing": true, "tts_provider_switched": true, "session_time_limit_exceeded": true,
	"insufficient_audio_activity": true, "final_timeout": true,
}

// Metrics holds every provider and cost metric (spec 8.8.1, 8.7.4). Labels never carry
// session or user identifiers.
type Metrics struct {
	requests          *prometheus.CounterVec
	errors            *prometheus.CounterVec
	latency           *prometheus.HistogramVec
	active            *prometheus.GaugeVec
	concurrencyWait   *prometheus.HistogramVec
	concurrencyReject *prometheus.CounterVec
	fallback          *prometheus.CounterVec
	circuitState      *prometheus.GaugeVec
	circuitTransition *prometheus.CounterVec
	streamDisconnects *prometheus.CounterVec
	ratelimitRemain   *prometheus.GaugeVec
	sttReplay         *prometheus.HistogramVec
	sttRotations      *prometheus.CounterVec
	ttsEpochRestarts  *prometheus.CounterVec
	usage             *prometheus.CounterVec
	cost              *prometheus.CounterVec
	sessionCost       prometheus.Histogram
	sessionCostPerMin prometheus.Histogram
}

// NewMetrics registers the metrics on reg (nil → a private registry, for tests).
func NewMetrics(reg prometheus.Registerer) *Metrics {
	if reg == nil {
		reg = prometheus.NewRegistry()
	}
	f := func(c prometheus.Collector) { reg.MustRegister(c) }
	m := &Metrics{
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "kotoki_provider_requests_total", Help: "Provider calls by outcome."},
			[]string{"provider", "capability", "model", "task", "outcome"}),
		errors: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "kotoki_provider_errors_total", Help: "Provider errors by kind."},
			[]string{"provider", "capability", "kind", "vendor_code"}),
		latency: prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "kotoki_provider_latency_seconds", Help: "Provider latency by phase.", Buckets: latencyBuckets},
			[]string{"provider", "capability", "model", "phase"}),
		active: prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "kotoki_provider_active", Help: "In-flight streams/requests."},
			[]string{"provider", "capability"}),
		concurrencyWait: prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "kotoki_provider_concurrency_wait_seconds", Help: "Wait for a local concurrency slot.", Buckets: latencyBuckets},
			[]string{"provider", "capability"}),
		concurrencyReject: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "kotoki_provider_concurrency_rejected_total", Help: "Calls rejected by the local concurrency limit."},
			[]string{"provider", "capability"}),
		fallback: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "kotoki_provider_fallback_total", Help: "TTS fallbacks to the character's fallback voice."},
			[]string{"capability", "from", "to", "reason"}),
		circuitState: prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "kotoki_provider_circuit_state", Help: "0 CLOSED, 1 OPEN, 2 HALF_OPEN."},
			[]string{"provider", "capability"}),
		circuitTransition: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "kotoki_provider_circuit_transitions_total", Help: "Circuit breaker transitions."},
			[]string{"provider", "capability", "to"}),
		streamDisconnects: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "kotoki_provider_stream_disconnects_total", Help: "Unexpected stream disconnects."},
			[]string{"provider", "capability"}),
		ratelimitRemain: prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "kotoki_provider_ratelimit_remaining", Help: "Vendor rate-limit headroom when reported."},
			[]string{"provider", "capability", "kind"}),
		sttReplay: prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "kotoki_stt_replay_seconds", Help: "Audio re-sent on STT connection rotation.", Buckets: latencyBuckets},
			[]string{"provider"}),
		sttRotations: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "kotoki_stt_rotations_total", Help: "STT connection rotations on the same vendor."},
			[]string{"provider"}),
		ttsEpochRestarts: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "kotoki_tts_epoch_restarts_total", Help: "TTS epoch restarts (fallback voice)."},
			[]string{"from", "to"}),
		usage: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "kotoki_provider_usage_total", Help: "Provider usage by unit."},
			[]string{"provider", "capability", "model", "unit"}),
		cost: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "kotoki_provider_cost_usd_total", Help: "Estimated provider cost in USD."},
			[]string{"provider", "capability", "model"}),
		sessionCost: prometheus.NewHistogram(prometheus.HistogramOpts{Name: "kotoki_session_cost_usd", Help: "Estimated cost per terminal session in USD.",
			Buckets: []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2}}),
		sessionCostPerMin: prometheus.NewHistogram(prometheus.HistogramOpts{Name: "kotoki_session_cost_per_minute_usd", Help: "Estimated cost per active minute in USD.",
			Buckets: []float64{0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5}}),
	}
	for _, c := range []prometheus.Collector{m.requests, m.errors, m.latency, m.active, m.concurrencyWait, m.concurrencyReject, m.fallback,
		m.circuitState, m.circuitTransition, m.streamDisconnects, m.ratelimitRemain, m.sttReplay, m.sttRotations, m.ttsEpochRestarts,
		m.usage, m.cost, m.sessionCost, m.sessionCostPerMin} {
		f(c)
	}
	return m
}

func seconds(d time.Duration) float64 { return d.Seconds() }

// ObserveCall counts one finished call; failures also bump the error counter.
func (m *Metrics) ObserveCall(p Name, c Capability, model, task, outcome string, kind ErrorKind, vendorCode string) {
	if m == nil {
		return
	}
	m.requests.WithLabelValues(string(p), string(c), model, task, outcome).Inc()
	if kind == "" {
		return
	}
	if !knownVendorCodes[vendorCode] {
		if vendorCode != "" {
			vendorCode = "other"
		}
	}
	m.errors.WithLabelValues(string(p), string(c), string(kind), vendorCode).Inc()
}

// ObserveLatency records one phase (connect, first_partial, final_after_finalize,
// first_token, first_sentence, total, first_audio).
func (m *Metrics) ObserveLatency(p Name, c Capability, model, phase string, d time.Duration) {
	if m == nil {
		return
	}
	m.latency.WithLabelValues(string(p), string(c), model, phase).Observe(seconds(d))
}

// ObserveFallback counts a TTS fallback; the capability label is always TTS.
func (m *Metrics) ObserveFallback(from, to Name, reason string) {
	if m == nil {
		return
	}
	m.fallback.WithLabelValues(string(CapTTS), string(from), string(to), reason).Inc()
}

// SetCircuit sets the circuit gauge.
func (m *Metrics) SetCircuit(p Name, c Capability, s BreakerState) {
	if m == nil {
		return
	}
	m.circuitState.WithLabelValues(string(p), string(c)).Set(float64(s))
}

// CircuitTransition counts a breaker transition and updates the gauge.
func (m *Metrics) CircuitTransition(p Name, c Capability, to BreakerState) {
	if m == nil {
		return
	}
	m.circuitTransition.WithLabelValues(string(p), string(c), to.String()).Inc()
	m.SetCircuit(p, c, to)
}

// Usage adds vendor usage (unit ∈ input_tokens, output_tokens, characters, audio_ms, requests).
func (m *Metrics) Usage(p Name, c Capability, model, unit string, n float64) {
	if m == nil || n <= 0 {
		return
	}
	m.usage.WithLabelValues(string(p), string(c), model, unit).Add(n)
}

// CostUSD adds estimated cost.
func (m *Metrics) CostUSD(p Name, c Capability, model string, usd float64) {
	if m == nil || usd <= 0 {
		return
	}
	m.cost.WithLabelValues(string(p), string(c), model).Add(usd)
}

// ObserveSessionCost records the cost of a terminal session (no labels, D-016).
func (m *Metrics) ObserveSessionCost(totalUSD, perMinuteUSD float64) {
	if m == nil {
		return
	}
	m.sessionCost.Observe(totalUSD)
	m.sessionCostPerMin.Observe(perMinuteUSD)
}

// ActiveInc and ActiveDec track in-flight streams/requests.
func (m *Metrics) ActiveInc(p Name, c Capability) {
	if m != nil {
		m.active.WithLabelValues(string(p), string(c)).Inc()
	}
}

// ActiveDec is the counterpart of ActiveInc.
func (m *Metrics) ActiveDec(p Name, c Capability) {
	if m != nil {
		m.active.WithLabelValues(string(p), string(c)).Dec()
	}
}

// ConcurrencyWait records the wait for a local slot.
func (m *Metrics) ConcurrencyWait(p Name, c Capability, d time.Duration) {
	if m != nil {
		m.concurrencyWait.WithLabelValues(string(p), string(c)).Observe(seconds(d))
	}
}

// ConcurrencyRejected counts a call rejected by the local limit.
func (m *Metrics) ConcurrencyRejected(p Name, c Capability) {
	if m != nil {
		m.concurrencyReject.WithLabelValues(string(p), string(c)).Inc()
	}
}

// StreamDisconnect counts an unexpected disconnect.
func (m *Metrics) StreamDisconnect(p Name, c Capability) {
	if m != nil {
		m.streamDisconnects.WithLabelValues(string(p), string(c)).Inc()
	}
}

// RateLimitRemaining publishes vendor headroom (kind ∈ requests, tokens).
func (m *Metrics) RateLimitRemaining(p Name, c Capability, kind string, v float64) {
	if m != nil {
		m.ratelimitRemain.WithLabelValues(string(p), string(c), kind).Set(v)
	}
}

// STTReplay records audio re-sent on a rotation.
func (m *Metrics) STTReplay(p Name, secs float64) {
	if m != nil {
		m.sttReplay.WithLabelValues(string(p)).Observe(secs)
	}
}

// STTRotation counts a same-vendor connection rotation.
func (m *Metrics) STTRotation(p Name) {
	if m != nil {
		m.sttRotations.WithLabelValues(string(p)).Inc()
	}
}

// TTSEpochRestart counts a TTS epoch restart.
func (m *Metrics) TTSEpochRestart(from, to Name) {
	if m != nil {
		m.ttsEpochRestarts.WithLabelValues(string(from), string(to)).Inc()
	}
}

// ObserveLLMFirstSentence records the first_sentence phase (observed by the runtime).
func (m *Metrics) ObserveLLMFirstSentence(p Name, model string, d time.Duration) {
	m.ObserveLatency(p, CapLLM, model, "first_sentence", d)
}

var defaultMetrics atomic.Pointer[Metrics]

// SetDefaultMetrics installs the process-wide Metrics used by the package-level helpers.
func SetDefaultMetrics(m *Metrics) { defaultMetrics.Store(m) }

// ObserveLLMFirstSentence is the spec 8.8.1 helper for the runtime: it records on the
// default Metrics (a no-op before SetDefaultMetrics).
func ObserveLLMFirstSentence(p Name, model string, d time.Duration) {
	defaultMetrics.Load().ObserveLLMFirstSentence(p, model, d)
}
