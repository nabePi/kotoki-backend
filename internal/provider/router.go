package provider

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sort"
	"time"

	"github.com/google/uuid"

	"github.com/nabePi/kotoki-backend/internal/platform/clock"
)

// ProcessRole selects wait and retry policy: realtime pods wait 100 ms for an LLM/TTS slot
// and retry once at most; workers wait 30 s and retry with backoff (spec 8.5.6, 8.6.2).
type ProcessRole string

// Process roles.
const (
	RoleRealtime ProcessRole = "realtime"
	RoleWorker   ProcessRole = "worker"
)

// RealtimeRouter is used by the conversation runtime (Bab 7). The runtime calls LLM/TTS
// only through Generation so the TRUNCATED and TTS fallback rules hold.
type RealtimeRouter interface {
	// OpenSTT opens a stream on the active STT vendor (8.5.8); a connect failure is a *Error.
	OpenSTT(ctx context.Context, cfg STTSessionConfig) (RoutedSTTSession, error)
	// NewGeneration creates the context of one AI generation (one `generation` value, D-073).
	NewGeneration(sessionID uuid.UUID, generation int64, voice VoiceSelection) Generation
	// CompleteText is a non-streaming text completion outside a generation (rolling summary).
	CompleteText(ctx context.Context, req TextRequest) (TextResult, error)
}

// TextRequest is a plain-text (not JSON) non-streaming LLM completion.
type TextRequest struct {
	Task        string // "ROLLING_SUMMARY" | "SUMMARY": metric label
	Model       string // optional override; empty → the active LLM vendor's model
	Messages    []Message
	MaxTokens   int // >0
	Temperature float64
	RequestID   string // "{task}:{session_id}"
}

// TextResult is the collected completion.
type TextResult struct {
	Text         string
	Usage        TokenUsage
	FinishReason FinishReason
	Provider     Name
	Model        string
	Latency      time.Duration
}

// RoutedSTTSession is an STTSession wrapped by the router (breaker, semaphore, logical
// offsets, same-vendor connection rotation).
type RoutedSTTSession interface {
	STTSession
	Provider() Name
	// ReportFinalTimeout is called when FINAL{FromFinalize} misses PROVIDER_TIMEOUT_STT_FINAL;
	// three in a row make the breaker count a failure and the router emit
	// STTError{TIMEOUT, "final_timeout"}. An on-time FINAL resets the count.
	ReportFinalTimeout()
}

// Generation wraps every LLM and TTS call of one generation. LLM: the single active vendor,
// no fallback. TTS: the character's voice; the switch to voice.fallback happens only before
// audio was sent.
type Generation interface {
	StreamLLM(ctx context.Context, req LLMRequest) (LLMStream, error)
	Synthesize(ctx context.Context, req TTSRequest) (TTSStream, error)
	// MarkAudioSent is called by the pacer right before the first downstream audio frame of
	// this generation is queued for the WebSocket writer (S-12). From then on TTS fallback is
	// forbidden and an LLM deadline ends the stream with DONE{TRUNCATED}.
	MarkAudioSent()
	// TTSEpochChanged delivers the new epoch when TTS moved to the character's fallback voice;
	// the runtime drops chunks of older epochs and resends every sentence of the generation.
	TTSEpochChanged() <-chan int
	Summary() GenerationSummary
}

// GenerationSummary feeds session_turns columns and metrics.
type GenerationSummary struct {
	LLMProvider     Name
	LLMModel        string
	TTSProvider     Name // vendor whose audio was sent; "" before any audio
	TTSModel        string
	TTSVoiceID      string // "" before any audio
	UsedFallback    bool
	LLMUsage        TokenUsage
	TTSCharacters   int // sum over ALL TTS requests, including epoch restarts
	LLMFirstTokenMs int64
	TTSFirstAudioMs int64
}

// AsyncRouter is used by workers (Bab 9–12).
type AsyncRouter interface {
	// GenerateJSON retries with backoff on the active evaluation vendor (8.6.2).
	GenerateJSON(ctx context.Context, req StructuredRequest) (StructuredResult, error)
	// Evaluation exposes the wrapped provider for callers that run their own call sequence
	// (Bab 10: main call → repair, at most 2 LLM calls per delivery): exactly one attempt per call.
	Evaluation() EvaluationProvider
	// CompleteText serves the session summary (Bab 9).
	CompleteText(ctx context.Context, req TextRequest) (TextResult, error)
	Embed(ctx context.Context, req EmbeddingRequest) (EmbeddingResult, error)
}

// RouterDeps are the adapters and collaborators wired by cmd/* (D-004).
type RouterDeps struct {
	STT        STTProvider          // exactly one (or the fake)
	LLM        LLMProvider          // exactly one (or the fake)
	Evaluation EvaluationProvider   // same vendor as LLM
	TTS        map[Name]TTSProvider // 1–2 enabled TTS vendors (or FAKE_PRIMARY + FAKE_FALLBACK)
	Embedding  EmbeddingProvider
	Prices     PriceTable
	Clock      clock.Clock
	Log        *slog.Logger
	Role       ProcessRole
	Metrics    *Metrics
	// Sleep and Jitter default to a real timer and ±20 % random jitter; tests inject fakes.
	Sleep  func(ctx context.Context, d time.Duration) error
	Jitter func(d time.Duration) time.Duration
	// AsyncWait is how long an async call (worker LLM/evaluation/embedding) waits for a
	// concurrency slot; default 30 s.
	AsyncWait time.Duration
}

type key struct {
	Provider   Name
	Capability Capability
}

// Router routes calls to the single active vendor per capability (TTS: per character
// voice), guarded by a breaker and a semaphore per (vendor, capability).
type Router struct {
	cfg      RouterConfig
	role     ProcessRole
	stt      STTProvider
	llm      LLMProvider
	eval     EvaluationProvider
	tts      map[Name]TTSProvider
	embed    EmbeddingProvider
	breakers map[key]*Breaker
	sems     map[key]*semaphoreSlot
	health   *HealthTracker
	metrics  *Metrics
	prices   PriceTable
	clock    clock.Clock
	log      *slog.Logger
	sleep    func(ctx context.Context, d time.Duration) error
	jitter   func(d time.Duration) time.Duration
}

const (
	realtimeSlotWait = 100 * time.Millisecond
	asyncSlotWait    = 30 * time.Second
)

// NewRouter validates cfg and deps and builds the breakers and semaphores.
func NewRouter(cfg RouterConfig, d RouterDeps) (*Router, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	var missing []string
	for _, c := range []struct {
		name string
		nil  bool
	}{{"STT", d.STT == nil}, {"LLM", d.LLM == nil}, {"Evaluation", d.Evaluation == nil}, {"Embedding", d.Embedding == nil}} {
		if c.nil {
			missing = append(missing, c.name)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("provider: RouterDeps missing adapter(s): %v", missing)
	}
	if len(d.TTS) == 0 {
		return nil, errors.New("provider: RouterDeps.TTS must hold at least one TTS adapter")
	}
	for name, p := range d.TTS {
		if p == nil {
			return nil, fmt.Errorf("provider: RouterDeps.TTS[%s] is nil", name)
		}
	}
	r := &Router{
		cfg: cfg, role: d.Role, stt: d.STT, llm: d.LLM, eval: d.Evaluation, tts: d.TTS, embed: d.Embedding,
		breakers: map[key]*Breaker{}, sems: map[key]*semaphoreSlot{}, health: nil, metrics: d.Metrics, prices: d.Prices,
		clock: d.Clock, log: d.Log, sleep: d.Sleep, jitter: d.Jitter,
	}
	if r.role == "" {
		r.role = RoleRealtime
	}
	if r.clock == nil {
		r.clock = clock.System{}
	}
	if r.log == nil {
		r.log = slog.New(slog.DiscardHandler)
	}
	if r.sleep == nil {
		r.sleep = sleepCtx
	}
	if r.jitter == nil {
		r.jitter = randomJitter
	}
	r.health = NewHealthTracker(r.clock)

	asyncWait := d.AsyncWait
	if asyncWait <= 0 {
		asyncWait = asyncSlotWait
	}
	llmWait, ttsWait := realtimeSlotWait, realtimeSlotWait
	if r.role == RoleWorker {
		llmWait, ttsWait = asyncWait, asyncWait
	}
	r.register(d.STT.Name(), CapSTT, 0)
	r.register(d.LLM.Name(), CapLLM, llmWait)
	r.register(d.Evaluation.Name(), CapEvaluation, asyncWait)
	r.register(d.Embedding.Name(), CapEmbedding, asyncWait)
	for name := range d.TTS {
		r.register(name, CapTTS, ttsWait)
	}
	return r, nil
}

func (r *Router) register(p Name, c Capability, wait time.Duration) {
	k := key{p, c}
	if _, dup := r.breakers[k]; dup {
		return
	}
	r.breakers[k] = newBreaker(BreakerConfig{
		Threshold: r.cfg.CBFailureThreshold, Window: r.cfg.CBWindow, OpenDuration: r.cfg.CBOpenDuration, HalfOpenProbes: r.cfg.CBHalfOpenProbes,
	}, r.clock, func(from, to BreakerState) {
		r.metrics.CircuitTransition(p, c, to)
		r.log.Warn("provider circuit state changed", "provider", string(p), "capability", string(c), "from", from.String(), "to", to.String())
	})
	r.sems[k] = newSemaphoreSlot(r.cfg.Concurrency.Limit(p, c), wait)
	r.health.Touch(p, c)
	r.metrics.SetCircuit(p, c, BreakerClosed)
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// randomJitter spreads a backoff by ±20 %.
func randomJitter(d time.Duration) time.Duration {
	return time.Duration(float64(d) * (0.8 + 0.4*rand.Float64())) //nolint:gosec // jitter, not security-sensitive
}

// active returns the single adapter name of a non-TTS capability.
func (r *Router) active(c Capability) Name {
	switch c {
	case CapSTT:
		return r.stt.Name()
	case CapLLM:
		return r.llm.Name()
	case CapEvaluation:
		return r.eval.Name()
	case CapEmbedding:
		return r.embed.Name()
	}
	return ""
}

func circuitOpen(p Name, c Capability) *Error {
	e := NewCircuitOpen(c)
	e.Provider = p
	return e
}

// admit guards a call to the single active vendor of c: breaker first (fail-fast when
// open, no other vendor is tried), then the local concurrency semaphore. release frees the
// slot; the ticket must be completed with Done by the caller.
func (r *Router) admit(ctx context.Context, c Capability) (Ticket, func(), *Error) {
	p := r.active(c)
	return r.admitVendor(ctx, p, c)
}

func (r *Router) admitVendor(ctx context.Context, p Name, c Capability) (Ticket, func(), *Error) {
	k := key{p, c}
	tk, ok := r.breakers[k].Allow()
	if !ok {
		r.metrics.ObserveCall(p, c, "", "", "error", KindUnavailable, "circuit_open")
		return Ticket{}, nil, circuitOpen(p, c)
	}
	start := r.clock.Now()
	rel, err := r.sems[k].Acquire(ctx, p, c)
	r.metrics.ConcurrencyWait(p, c, r.clock.Now().Sub(start))
	if err != nil {
		tk.Done(err) // local_concurrency and cancellation are not counted by the breaker
		if err.VendorCode == vendorCodeLocal {
			r.metrics.ConcurrencyRejected(p, c)
		}
		return Ticket{}, nil, err
	}
	r.health.IncActive(p, c)
	r.metrics.ActiveInc(p, c)
	return tk, func() {
		r.health.DecActive(p, c)
		r.metrics.ActiveDec(p, c)
		rel()
	}, nil
}

// ttsCandidate is one voice a TTS sentence may be synthesised with.
type ttsCandidate struct {
	Ref VoiceRef
	// ViaCircuitOpen marks a fallback chosen because the primary vendor's breaker is open.
	ViaCircuitOpen bool
}

// ttsCandidates lists the usable voices in order: the primary, then the character's fallback
// when it exists and PROVIDER_TTS_FALLBACK_ENABLED. Breakers are per (vendor, capability),
// not per character (D-043). A vendor that is not registered is skipped; with nothing left
// the call fails (voice_binding_missing) or, when only open breakers remain, circuit_open.
func (r *Router) ttsCandidates(v VoiceSelection) ([]ttsCandidate, error) {
	refs := []VoiceRef{v.Primary}
	if v.Fallback != nil && r.cfg.TTSFallbackEnabled {
		refs = append(refs, *v.Fallback)
	}
	var out []ttsCandidate
	registered, primarySkippedOpen := 0, false
	for i, ref := range refs {
		if _, ok := r.tts[ref.Provider]; !ok {
			continue
		}
		registered++
		if !r.breakers[key{ref.Provider, CapTTS}].WouldAllow() {
			if i == 0 {
				primarySkippedOpen = true
			}
			continue
		}
		out = append(out, ttsCandidate{Ref: ref, ViaCircuitOpen: i > 0 && primarySkippedOpen})
	}
	switch {
	case len(out) > 0:
		return out, nil
	case registered == 0:
		e := NewError(v.Primary.Provider, CapTTS, KindInvalidRequest, "voice binding refers to a TTS vendor that is not enabled")
		e.VendorCode = "voice_binding_missing"
		return nil, e
	default:
		return nil, circuitOpen(v.Primary.Provider, CapTTS)
	}
}

// HealthSnapshot returns one row per registered (vendor, capability) for the debug endpoint.
func (r *Router) HealthSnapshot() []ProviderHealth {
	return r.health.Snapshot(
		func(p Name, c Capability) int { return r.sems[key{p, c}].Limit() },
		func(p Name, c Capability) BreakerState { return r.breakers[key{p, c}].State() },
	)
}

// llmModel is the model of the active LLM vendor.
func (r *Router) llmModel() string {
	switch r.llm.Name() {
	case DeepSeek:
		return r.cfg.DeepSeekLLMModel
	case OpenAI:
		return r.cfg.OpenAILLMModel
	}
	return r.cfg.LLMModel() // fake or unknown: follow the flags
}

// evaluationModel is the evaluation model of the active vendor.
func (r *Router) evaluationModel() string {
	switch r.eval.Name() {
	case DeepSeek:
		return r.cfg.DeepSeekEvaluationModel
	case OpenAI:
		return r.cfg.OpenAIEvaluationModel
	}
	return r.cfg.EvaluationModel()
}

// textModel picks the model of a CompleteText call: an explicit override, then the rolling
// summary model for ROLLING_SUMMARY, else the active LLM model.
func (r *Router) textModel(req TextRequest) string {
	switch {
	case req.Model != "":
		return req.Model
	case req.Task == "ROLLING_SUMMARY" && r.cfg.RollingSummaryLLMModel != "":
		return r.cfg.RollingSummaryLLMModel
	}
	return r.llmModel()
}

// ttsVendors lists the registered TTS vendors (for the content validator, Bab 6 §6.9).
func (r *Router) ttsVendors() []Name {
	out := make([]Name, 0, len(r.tts))
	for n := range r.tts {
		out = append(out, n)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
