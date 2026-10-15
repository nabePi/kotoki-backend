package conversation

import (
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/nabePi/kotoki-backend/internal/platform/metrics"
)

// Metrics holds the realtime metrics (spec §7.14, prefix kotoki_realtime_) plus the two runtime
// counters outside the prefix (kotoki_llm_content_filtered_total, kotoki_content_opening_fallback_total).
// Labels never carry session_id or user_id. Every method is safe on a nil *Metrics, so tests and
// partial wiring may pass nil.
//
// The unexported methods are the ones the connection plumbing and the generation pipeline of
// Tasks 10–12 call; the exported ones are the recording points of Runtime, the Manager and the
// tasks that extend them (turn engine, barge-in, disconnect/resume, end, drain): each one
// documents which spec row it feeds, so a later task finds the hook instead of adding a metric.
type Metrics struct {
	// §7.14 rows used by the plumbing since Task 10.
	audioFramesIn      *prometheus.CounterVec
	audioFramesOut     *prometheus.CounterVec
	protocolViolations *prometheus.CounterVec
	slowConsumer       prometheus.Counter
	outboundDropped    *prometheus.CounterVec
	aiSentenceLimit    *prometheus.CounterVec
	llmTruncated       prometheus.Counter
	difficultyAdjusted prometheus.Counter
	fenceLost          prometheus.Counter

	// Sessions, connections, capacity.
	sessionsActive  *prometheus.GaugeVec
	sessionCapacity prometheus.Gauge
	draining        prometheus.Gauge
	connections     *prometheus.CounterVec
	wsClose         *prometheus.CounterVec
	reconnects      *prometheus.CounterVec
	reconnectGap    prometheus.Histogram
	drainDuration   prometheus.Histogram

	// Turn latency.
	turnEndToFirstAudio *prometheus.HistogramVec
	sttFinal            *prometheus.HistogramVec
	llmFirstToken       *prometheus.HistogramVec
	llmFirstSentence    *prometheus.HistogramVec
	ttsFirstAudio       *prometheus.HistogramVec
	serverOverhead      prometheus.Histogram

	// Barge-in, turns, generation.
	bargeinStop      prometheus.Histogram
	bargeins         *prometheus.CounterVec
	turns            *prometheus.CounterVec
	turnEndReason    *prometheus.CounterVec
	turnResumed      prometheus.Counter
	genCancelled     *prometheus.CounterVec
	genExit          prometheus.Histogram
	sttDiscarded     *prometheus.CounterVec
	providerFallback *prometheus.CounterVec
	playbackMissing  prometheus.Counter
	rollingSummary   *prometheus.CounterVec
	frameProcessing  *prometheus.HistogramVec
	outboundDepth    *prometheus.HistogramVec
	promptTokens     *prometheus.HistogramVec
	promptEstimate   prometheus.Histogram
	llmContentFilter *prometheus.CounterVec
	openingFallback  prometheus.Counter
}

// Bucket sets of §7.14 that Chapter 13 does not name.
var (
	// frameProcessingBuckets is the CPU cost of one frame (given by §7.14).
	frameProcessingBuckets = []float64{0.0001, 0.0002, 0.0005, 0.001, 0.002, 0.005, 0.01, 0.02}
	// promptTokenBuckets is the token bucket set of §7.14.
	promptTokenBuckets = []float64{50, 100, 200, 400, 800, 1600, 3200, 6400}
	// queueDepthBuckets covers 0..WS_OUTBOUND_QUEUE (256); §7.14 gives none.
	queueDepthBuckets = []float64{0, 1, 2, 4, 8, 16, 32, 64, 128, 256}
	// estimateRatioBuckets centres on 1 (estimate == provider usage); §7.14 gives none.
	estimateRatioBuckets = []float64{0.5, 0.75, 0.9, 1, 1.1, 1.25, 1.5, 2}
	// drainBuckets covers the 20 s safepoint plus the 5 s flush; §7.14 gives none.
	drainBuckets = []float64{0.5, 1, 2, 5, 10, 20, 25, 30}
)

const metricPrefix = "kotoki_realtime_"

// NewMetrics registers the metrics on reg (nil → a private registry, for tests).
func NewMetrics(reg prometheus.Registerer) *Metrics {
	if reg == nil {
		reg = prometheus.NewRegistry()
	}
	counter := func(name, help string, labels ...string) *prometheus.CounterVec {
		return prometheus.NewCounterVec(prometheus.CounterOpts{Name: metricPrefix + name, Help: help}, labels)
	}
	plainCounter := func(name, help string) prometheus.Counter {
		return prometheus.NewCounter(prometheus.CounterOpts{Name: metricPrefix + name, Help: help})
	}
	hist := func(name, help string, buckets []float64, labels ...string) *prometheus.HistogramVec {
		return prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: metricPrefix + name, Help: help, Buckets: buckets}, labels)
	}
	plainHist := func(name, help string, buckets []float64) prometheus.Histogram {
		return prometheus.NewHistogram(prometheus.HistogramOpts{Name: metricPrefix + name, Help: help, Buckets: buckets})
	}
	m := &Metrics{
		audioFramesIn:      counter("audio_frames_in_total", "Upstream audio frames by result.", "result"),
		audioFramesOut:     counter("audio_frames_out_total", "Downstream audio frames by result.", "result"),
		protocolViolations: counter("protocol_violations_total", "Connections closed 1008 by violation kind.", "kind"),
		slowConsumer:       plainCounter("slow_consumer_total", "Connections dropped because the outbound queue was full."),
		outboundDropped: counter("outbound_transient_dropped_total",
			"Transient control messages replaced by a newer one (coalesced) or dropped.", "reason"),
		aiSentenceLimit:    counter("ai_sentence_limit_total", "Generations stopped by the R-17 sentence limit.", "kind"),
		llmTruncated:       plainCounter("llm_truncated_total", "LLM streams the router ended with DONE{TRUNCATED} after audio was sent (S-7)."),
		difficultyAdjusted: plainCounter("difficulty_adjustments_total", "In-session adaptive difficulty adjustments (spec 7.8.6)."),
		fenceLost: plainCounter("fence_lost_total",
			"Runtimes that lost ownership of their session (ErrFenceLost or owner refresh refused, spec 7.9.3)."),

		sessionsActive: prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: metricPrefix + "sessions_active",
			Help: "Runtimes owned by the pod by state; the sum is activeSessions (KEDA metric)."}, []string{"state"}),
		sessionCapacity: prometheus.NewGauge(prometheus.GaugeOpts{Name: metricPrefix + "session_capacity",
			Help: "REALTIME_MAX_SESSIONS."}),
		draining:    prometheus.NewGauge(prometheus.GaugeOpts{Name: metricPrefix + "draining", Help: "1 while the pod drains."}),
		connections: counter("connections_total", "Handshake results (spec 7.3.1).", "result"),
		wsClose:     counter("ws_close_total", "WebSocket close codes sent.", "code"),
		reconnects:  counter("reconnect_total", "Reconnect outcomes.", "result", "replay"),
		reconnectGap: plainHist("reconnect_gap_seconds", "Time from the disconnect to the attach of the next connection.",
			metrics.TurnBuckets),
		drainDuration: plainHist("drain_duration_seconds", "Duration of a pod drain.", drainBuckets),

		turnEndToFirstAudio: hist("turn_end_to_first_audio_seconds",
			"Turn end detected (opening: ACTIVE) to the first audio frame queued for the writer.", metrics.TurnBuckets, "kind", "used_fallback"),
		sttFinal:         hist("stt_final_seconds", "Finalize() to FINAL{FromFinalize}.", metrics.TurnBuckets, "provider"),
		llmFirstToken:    hist("llm_first_token_seconds", "LLM request to the first delta.", metrics.TurnBuckets, "provider", "model"),
		llmFirstSentence: hist("llm_first_sentence_seconds", "LLM request to the first parsed sentence.", metrics.TurnBuckets, "provider", "model"),
		ttsFirstAudio:    hist("tts_first_audio_seconds", "TTS request of the first sentence to the first PCM chunk.", metrics.TurnBuckets, "provider", "model"),
		serverOverhead: plainHist("server_overhead_seconds",
			"turn_end_to_first_audio minus stt_final, llm_first_sentence and tts_first_audio of a REPLY turn.", metrics.TurnBuckets),

		bargeinStop: plainHist("bargein_stop_seconds", "Barge-in confirmed to the last audio frame written (D-101).", metrics.BargeBuckets),
		bargeins:    counter("bargein_total", "Barge-ins by source.", "source"),
		turns:       counter("turns_total", "Finished turns by status.", "status"),
		turnEndReason: counter("turn_end_reason_total",
			"Turn ends by the silence threshold or limit that fired.", "reason"),
		turnResumed:  plainCounter("turn_resumed_total", "User resumed speaking in PROCESSING (proxy of false turn ends)."),
		genCancelled: counter("generation_cancelled_total", "Cancelled generations by ai.interrupted reason.", "reason"),
		genExit: plainHist("generation_goroutine_exit_seconds", "Cancel to every goroutine of the generation gone.",
			metrics.BargeBuckets),
		sttDiscarded: counter("stt_discarded_commits_total", "DISCARD commit FINALs dropped (S-8); the text is never recorded.", "boundary"),
		providerFallback: counter("provider_fallback_total", "TTS fallbacks to the character's voice.fallback.",
			"capability", "from", "to"),
		playbackMissing: plainCounter("playback_finished_missing_total", "Turns closed by the playback deadline, not by the client."),
		rollingSummary:  counter("rolling_summary_total", "Rolling summary runs by result.", "result"),
		frameProcessing: hist("frame_processing_seconds", "CPU cost per frame by stage.", frameProcessingBuckets, "stage"),
		outboundDepth:   hist("outbound_queue_depth", "Outbound queue depth, sampled every second.", queueDepthBuckets, "queue"),
		promptTokens:    hist("prompt_tokens", "Estimated prompt tokens per component.", promptTokenBuckets, "component"),
		promptEstimate:  plainHist("prompt_token_estimate_ratio", "Estimated / provider-reported input tokens.", estimateRatioBuckets),
		llmContentFilter: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "kotoki_llm_content_filtered_total",
			Help: "Turns failed by CONTENT_FILTERED (S-9)."}, []string{"provider"}),
		openingFallback: prometheus.NewCounter(prometheus.CounterOpts{Name: "kotoki_content_opening_fallback_total",
			Help: "Static opening played because no OPENING prompt version was PUBLISHED (S-2)."}),
	}
	reg.MustRegister(m.collectors()...)
	return m
}

// collectors lists every collector once; NewMetrics registers them and the tests read their
// descriptors.
func (m *Metrics) collectors() []prometheus.Collector {
	return []prometheus.Collector{
		m.audioFramesIn, m.audioFramesOut, m.protocolViolations, m.slowConsumer, m.outboundDropped, m.aiSentenceLimit,
		m.llmTruncated, m.difficultyAdjusted, m.fenceLost,
		m.sessionsActive, m.sessionCapacity, m.draining, m.connections, m.wsClose, m.reconnects, m.reconnectGap, m.drainDuration,
		m.turnEndToFirstAudio, m.sttFinal, m.llmFirstToken, m.llmFirstSentence, m.ttsFirstAudio, m.serverOverhead,
		m.bargeinStop, m.bargeins, m.turns, m.turnEndReason, m.turnResumed, m.genCancelled, m.genExit, m.sttDiscarded,
		m.providerFallback, m.playbackMissing, m.rollingSummary, m.frameProcessing, m.outboundDepth, m.promptTokens,
		m.promptEstimate, m.llmContentFilter, m.openingFallback,
	}
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

func (m *Metrics) fenceLostInc() {
	if m != nil {
		m.fenceLost.Inc()
	}
}

// Session states of sessions_active{state}.
const (
	SessionStarting = "starting"
	SessionActive   = "active"
	SessionDetached = "detached"
	SessionEnding   = "ending"
)

// SessionState moves one runtime from state "from" to state "to" in sessions_active ("" = not
// counted before/after: a runtime appearing or leaving).
func (m *Metrics) SessionState(from, to string) {
	if m == nil {
		return
	}
	if from != "" {
		m.sessionsActive.WithLabelValues(from).Dec()
	}
	if to != "" {
		m.sessionsActive.WithLabelValues(to).Inc()
	}
}

// SetSessionCapacity sets session_capacity (REALTIME_MAX_SESSIONS).
func (m *Metrics) SetSessionCapacity(n int) {
	if m != nil {
		m.sessionCapacity.Set(float64(n))
	}
}

// SetDraining sets draining (Task 23).
func (m *Metrics) SetDraining(on bool) {
	if m == nil {
		return
	}
	v := 0.0
	if on {
		v = 1
	}
	m.draining.Set(v)
}

// Handshake results of connections_total (§7.3.1). The gateway counts the first six (steps
// 1–6), the Manager the last two.
const (
	ConnAccepted            = "accepted"
	ConnRejectedOrigin      = "rejected_origin"
	ConnRejectedRateLimit   = "rejected_ratelimit"
	ConnRejectedDraining    = "rejected_draining"
	ConnRejectedCapacity    = "rejected_capacity"
	ConnRejectedTicket      = "rejected_ticket"
	ConnRejectedUnavailable = "rejected_unavailable"
	ConnRejectedState       = "rejected_state"
)

// Connection counts one handshake result.
func (m *Metrics) Connection(result string) {
	if m != nil {
		m.connections.WithLabelValues(result).Inc()
	}
}

// WSClose counts one close code sent to a client.
func (m *Metrics) WSClose(code int) {
	if m != nil {
		m.wsClose.WithLabelValues(strconv.Itoa(code)).Inc()
	}
}

// Reconnect counts one reconnect outcome (result: same_pod, takeover, timeout, rejected; replay:
// complete, unavailable) and, when gap > 0, reconnect_gap_seconds (Task 21).
func (m *Metrics) Reconnect(result, replay string, gap time.Duration) {
	if m == nil {
		return
	}
	m.reconnects.WithLabelValues(result, replay).Inc()
	if gap > 0 {
		m.reconnectGap.Observe(gap.Seconds())
	}
}

// DrainDuration observes drain_duration_seconds (Task 23).
func (m *Metrics) DrainDuration(d time.Duration) {
	if m != nil {
		m.drainDuration.Observe(d.Seconds())
	}
}

// TurnEndToFirstAudio observes turn_end_to_first_audio_seconds (kind: reply, opening).
func (m *Metrics) TurnEndToFirstAudio(kind string, usedFallback bool, d time.Duration) {
	if m != nil {
		m.turnEndToFirstAudio.WithLabelValues(kind, strconv.FormatBool(usedFallback)).Observe(d.Seconds())
	}
}

// STTFinal observes stt_final_seconds.
func (m *Metrics) STTFinal(provider string, d time.Duration) {
	if m != nil {
		m.sttFinal.WithLabelValues(provider).Observe(d.Seconds())
	}
}

// LLMFirstToken observes llm_first_token_seconds.
func (m *Metrics) LLMFirstToken(provider, model string, d time.Duration) {
	if m != nil {
		m.llmFirstToken.WithLabelValues(provider, model).Observe(d.Seconds())
	}
}

// LLMFirstSentence observes llm_first_sentence_seconds.
func (m *Metrics) LLMFirstSentence(provider, model string, d time.Duration) {
	if m != nil {
		m.llmFirstSentence.WithLabelValues(provider, model).Observe(d.Seconds())
	}
}

// TTSFirstAudio observes tts_first_audio_seconds.
func (m *Metrics) TTSFirstAudio(provider, model string, d time.Duration) {
	if m != nil {
		m.ttsFirstAudio.WithLabelValues(provider, model).Observe(d.Seconds())
	}
}

// ServerOverhead observes server_overhead_seconds (a REPLY turn only).
func (m *Metrics) ServerOverhead(d time.Duration) {
	if m != nil {
		m.serverOverhead.Observe(d.Seconds())
	}
}

// BargeInStop observes bargein_stop_seconds (Task 20).
func (m *Metrics) BargeInStop(d time.Duration) {
	if m != nil {
		m.bargeinStop.Observe(d.Seconds())
	}
}

// BargeIn counts bargein_total (source: server, hint_confirmed, hint_unconfirmed; Task 20).
func (m *Metrics) BargeIn(source string) {
	if m != nil {
		m.bargeins.WithLabelValues(source).Inc()
	}
}

// Turn counts turns_total (status: COMPLETED, INTERRUPTED, FAILED, DISCARDED).
func (m *Metrics) Turn(status string) {
	if m != nil {
		m.turns.WithLabelValues(status).Inc()
	}
}

// TurnEndReason counts turn_end_reason_total (silence_500, silence_800, silence_1400,
// silence_filler_only, max_utterance; Task 19).
func (m *Metrics) TurnEndReason(reason string) {
	if m != nil {
		m.turnEndReason.WithLabelValues(reason).Inc()
	}
}

// TurnResumed counts turn_resumed_total (row 9, Task 19).
func (m *Metrics) TurnResumed() {
	if m != nil {
		m.turnResumed.Inc()
	}
}

// GenerationCancelled counts generation_cancelled_total by the ai.interrupted reason.
func (m *Metrics) GenerationCancelled(reason string) {
	if m != nil {
		m.genCancelled.WithLabelValues(reason).Inc()
	}
}

// GenerationExit observes generation_goroutine_exit_seconds (Task 20).
func (m *Metrics) GenerationExit(d time.Duration) {
	if m != nil {
		m.genExit.Observe(d.Seconds())
	}
}

// STTDiscarded counts stt_discarded_commits_total (boundary: ai_speaking, listening).
func (m *Metrics) STTDiscarded(boundary string) {
	if m != nil {
		m.sttDiscarded.WithLabelValues(boundary).Inc()
	}
}

// ProviderFallback counts provider_fallback_total (only the TTS fallback exists, D-017).
func (m *Metrics) ProviderFallback(capability, from, to string) {
	if m != nil {
		m.providerFallback.WithLabelValues(capability, from, to).Inc()
	}
}

// PlaybackFinishedMissing counts playback_finished_missing_total.
func (m *Metrics) PlaybackFinishedMissing() {
	if m != nil {
		m.playbackMissing.Inc()
	}
}

// RollingSummary counts rolling_summary_total{result}.
func (m *Metrics) RollingSummary(result string) {
	if m != nil {
		m.rollingSummary.WithLabelValues(result).Inc()
	}
}

// Frame processing stages of frame_processing_seconds.
const (
	StageOpusDecode = "opus_decode"
	StageVAD        = "vad"
	StageOpusEncode = "opus_encode"
)

// FrameProcessing observes frame_processing_seconds{stage}.
func (m *Metrics) FrameProcessing(stage string, d time.Duration) {
	if m != nil {
		m.frameProcessing.WithLabelValues(stage).Observe(d.Seconds())
	}
}

// OutboundDepth observes outbound_queue_depth for one connection (sampled every second).
func (m *Metrics) OutboundDepth(control, audio int) {
	if m == nil {
		return
	}
	m.outboundDepth.WithLabelValues("control").Observe(float64(control))
	m.outboundDepth.WithLabelValues("audio").Observe(float64(audio))
}

// PromptTokens observes prompt_tokens{component} from BuiltPrompt.ComponentTokens.
func (m *Metrics) PromptTokens(component string, tokens int) {
	if m != nil {
		m.promptTokens.WithLabelValues(component).Observe(float64(tokens))
	}
}

// PromptEstimateRatio observes prompt_token_estimate_ratio (estimate / provider usage; a
// non-positive usage is skipped).
func (m *Metrics) PromptEstimateRatio(estimated, used int) {
	if m != nil && used > 0 {
		m.promptEstimate.Observe(float64(estimated) / float64(used))
	}
}

// LLMContentFiltered counts kotoki_llm_content_filtered_total{provider} (S-9).
func (m *Metrics) LLMContentFiltered(provider string) {
	if m != nil {
		m.llmContentFilter.WithLabelValues(provider).Inc()
	}
}

// OpeningFallback counts kotoki_content_opening_fallback_total (S-2).
func (m *Metrics) OpeningFallback() {
	if m != nil {
		m.openingFallback.Inc()
	}
}
