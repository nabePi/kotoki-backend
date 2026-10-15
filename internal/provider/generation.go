package provider

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

// generation is one AI generation: all LLM and TTS calls for one `generation` value (D-073).
// LLM uses the single active vendor with no fallback and no RESET event (D-017); TTS pins
// the voice per generation (generation_tts.go).
type generation struct {
	r         *Router
	sessionID uuid.UUID
	gen       int64
	voice     VoiceSelection
	audioSent atomic.Bool

	mu      sync.Mutex
	summary GenerationSummary

	// TTS state (generation_tts.go), guarded by mu
	ttsInit       bool // the initial slot (primary or fallback) has been pinned
	ttsOnFallback bool
	ttsEpoch      int
	epochCh       chan int
	ttsStreams    map[*ttsOut]struct{}
}

var _ Generation = (*generation)(nil)

// NewGeneration creates the context of one generation; voice is the character's voice from
// the session snapshot (D-015).
func (r *Router) NewGeneration(sessionID uuid.UUID, gen int64, voice VoiceSelection) Generation {
	return &generation{
		r: r, sessionID: sessionID, gen: gen, voice: voice,
		epochCh: make(chan int, 1), ttsStreams: map[*ttsOut]struct{}{},
		summary: GenerationSummary{LLMProvider: r.llm.Name(), LLMModel: r.llmModel()},
	}
}

// MarkAudioSent is called by the pacer right before the first downstream audio frame (S-12).
// It takes g.mu so that a TTS switch decided concurrently either sees the flag or completed
// before it (generation_tts.go, switchTTS).
func (g *generation) MarkAudioSent() {
	g.mu.Lock()
	g.audioSent.Store(true)
	g.mu.Unlock()
}

// TTSEpochChanged delivers the new epoch when TTS moved to the character's fallback voice.
func (g *generation) TTSEpochChanged() <-chan int { return g.epochCh }

// Summary returns a snapshot of the generation's outcome so far.
func (g *generation) Summary() GenerationSummary {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.summary
}

func (g *generation) update(f func(*GenerationSummary)) {
	g.mu.Lock()
	defer g.mu.Unlock()
	f(&g.summary)
}

// genLLMStream is the router-wrapped stream returned by StreamLLM.
type genLLMStream struct {
	out    chan LLMEvent
	ctx    context.Context
	cancel context.CancelFunc
	once   sync.Once
}

func (s *genLLMStream) Events() <-chan LLMEvent { return s.out }

func (s *genLLMStream) Close() error {
	s.once.Do(s.cancel)
	return nil
}

func messageRunes(msgs []Message) int {
	n := 0
	for _, m := range msgs {
		n += utf8.RuneCountInString(m.Content)
	}
	return n
}

func ctxError(p Name, c Capability, err error) *Error {
	if errors.Is(err, context.DeadlineExceeded) {
		return &Error{Provider: p, Capability: c, Kind: KindTimeout, Retryable: true, Cause: err, Message: "deadline exceeded"}
	}
	return &Error{Provider: p, Capability: c, Kind: KindCancelled, Cause: err, Message: "cancelled"}
}

// asError converts any error to a *Error for events and metrics.
func asError(p Name, c Capability, err error) *Error {
	var pe *Error
	if errors.As(err, &pe) {
		return pe
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return ctxError(p, c, err)
	}
	return &Error{Provider: p, Capability: c, Kind: KindUnavailable, Retryable: true, Cause: err, Message: "provider call failed"}
}

// StreamLLM calls the active LLM vendor. Failures arrive as an ERROR event, never as a
// vendor switch; a deadline after MarkAudioSent ends the stream with DONE{TRUNCATED} (S-7).
func (g *generation) StreamLLM(ctx context.Context, req LLMRequest) (LLMStream, error) {
	r := g.r
	req.Model = r.llmModel()
	if req.MaxTokens <= 0 {
		req.MaxTokens = r.cfg.LLMMaxOutputTokens
	}
	if req.Temperature < 0 {
		req.Temperature = r.cfg.LLMTemperature
	}
	sctx, cancel := context.WithCancel(ctx)
	s := &genLLMStream{out: make(chan LLMEvent, 128), ctx: sctx, cancel: cancel}
	go g.runLLM(ctx, sctx, s, req)
	return s, nil
}

// llmRun carries the bookkeeping of one StreamLLM call.
type llmRun struct {
	g        *generation
	s        *genLLMStream
	req      LLMRequest
	provider Name
	start    time.Time
	firstMs  int64
	sawFirst bool
	outRunes int
	ticket   Ticket
	release  func()
}

func (g *generation) runLLM(parent, sctx context.Context, s *genLLMStream, req LLMRequest) {
	r := g.r
	defer close(s.out)
	defer s.cancel()
	p := r.llm.Name()
	run := &llmRun{g: g, s: s, req: req, provider: p, start: r.clock.Now()}

	if err := parent.Err(); err != nil { // the session deadline already passed, or the caller left
		if errors.Is(err, context.DeadlineExceeded) {
			run.conclude("error", ctxError(p, CapLLM, err), nil, run.estimate())
			run.terminal(LLMEvent{Type: LLMError, Err: ctxError(p, CapLLM, err)})
		} else {
			run.cancelled()
		}
		return
	}
	tk, rel, aerr := r.admit(sctx, CapLLM)
	if aerr != nil {
		run.admitFailed(aerr)
		return
	}
	run.ticket, run.release = tk, rel

	attemptCtx, cancelAttempt := context.WithTimeout(sctx, r.cfg.TimeoutLLMTotal)
	defer cancelAttempt()
	stream, err := r.llm.Stream(attemptCtx, req)
	if err != nil {
		run.failed(asError(p, CapLLM, err))
		return
	}
	defer func() { _ = stream.Close() }()

	firstTimer := time.NewTimer(r.cfg.TimeoutLLMFirstToken)
	defer firstTimer.Stop()
	firstC := firstTimer.C

	for {
		select {
		case ev, ok := <-stream.Events():
			if !ok {
				run.failed(NewError(p, CapLLM, KindUnavailable, "stream closed without a terminal event"))
				return
			}
			switch ev.Type {
			case LLMDelta:
				if !run.sawFirst {
					run.sawFirst = true
					run.firstMs = r.clock.Now().Sub(run.start).Milliseconds()
					firstTimer.Stop()
					firstC = nil
				}
				run.outRunes += utf8.RuneCountInString(ev.Text)
				if !run.forward(ev) {
					run.cancelled()
					return
				}
			case LLMDone:
				if ev.FinishReason == FinishCancelled && attemptCtx.Err() != nil {
					run.contextEnded(parent, attemptCtx)
				} else {
					run.done(ev)
				}
				return
			case LLMError:
				if attemptCtx.Err() != nil {
					run.contextEnded(parent, attemptCtx)
				} else {
					run.failed(asError(p, CapLLM, ev.Err))
				}
				return
			}
		case <-firstC:
			cancelAttempt()
			run.failed(&Error{Provider: p, Capability: CapLLM, Kind: KindTimeout, VendorCode: "first_token_timeout", Retryable: true,
				Message: "no first token within the budget"})
			return
		case <-attemptCtx.Done():
			run.contextEnded(parent, attemptCtx)
			return
		}
	}
}

// forward passes a DELTA on; false when the consumer closed the stream.
func (run *llmRun) forward(ev LLMEvent) bool {
	select {
	case run.s.out <- ev:
		return true
	default:
	}
	select {
	case run.s.out <- ev:
		return true
	case <-run.s.ctx.Done():
		return false
	}
}

// terminal delivers the last event after the summary is final.
func (run *llmRun) terminal(ev LLMEvent) {
	if ev.Provider == "" {
		ev.Provider = run.provider
	}
	if ev.Model == "" {
		ev.Model = run.req.Model
	}
	ev.ReceivedAt = run.g.r.clock.Now()
	select {
	case run.s.out <- ev:
		return
	default:
	}
	select {
	case run.s.out <- ev:
	case <-time.After(time.Second): // nobody is reading
	}
}

func (run *llmRun) estimate() TokenUsage {
	return TokenUsage{InputTokens: messageRunes(run.req.Messages), OutputTokens: run.outRunes, Estimated: true}
}

// conclude settles breaker, semaphore, summary, metrics, health and the debug log exactly once.
func (run *llmRun) conclude(outcome string, kindErr *Error, ticketErr error, usage TokenUsage) {
	g, r, p := run.g, run.g.r, run.provider
	total := r.clock.Now().Sub(run.start)
	if run.release != nil {
		run.ticket.Done(ticketErr)
		run.release()
		run.release = nil
	}
	g.update(func(s *GenerationSummary) {
		s.LLMProvider, s.LLMModel, s.LLMUsage, s.LLMFirstTokenMs = p, run.req.Model, usage, run.firstMs
	})
	kind, code := ErrorKind(""), ""
	if kindErr != nil {
		kind, code = kindErr.Kind, kindErr.VendorCode
	}
	r.metrics.ObserveCall(p, CapLLM, run.req.Model, "", outcome, kind, code)
	if run.sawFirst {
		r.metrics.ObserveLatency(p, CapLLM, run.req.Model, "first_token", time.Duration(run.firstMs)*time.Millisecond)
	}
	r.metrics.ObserveLatency(p, CapLLM, run.req.Model, "total", total)
	r.metrics.Usage(p, CapLLM, run.req.Model, "input_tokens", float64(usage.InputTokens))
	r.metrics.Usage(p, CapLLM, run.req.Model, "output_tokens", float64(usage.OutputTokens))
	if r.prices != nil {
		if usd, ok := r.prices.LLM(p, CapLLM, run.req.Model, usage); ok {
			r.metrics.CostUSD(p, CapLLM, run.req.Model, usd)
		}
	}
	var healthErr error
	if outcome == "error" && kindErr != nil {
		healthErr = kindErr
	} else if outcome == "cancelled" {
		healthErr = context.Canceled
	}
	r.health.Record(p, CapLLM, healthErr, total)
	r.log.Debug("provider call", "provider", string(p), "capability", string(CapLLM), "model", run.req.Model, "request_id", run.req.RequestID,
		"session_id", g.sessionID.String(), "outcome", outcome, "kind", string(kind), "latency_first_ms", run.firstMs,
		"latency_total_ms", total.Milliseconds(), "input_tokens", usage.InputTokens, "output_tokens", usage.OutputTokens)
}

func (run *llmRun) done(ev LLMEvent) {
	usage := run.estimate()
	if ev.Usage != nil {
		usage = *ev.Usage
	}
	ev.Usage = &usage
	if ev.FinishReason == FinishContentFiltered { // the vendor refused: not a vendor failure
		kindErr := NewError(run.provider, CapLLM, KindContentFiltered, "response blocked by the content filter")
		run.conclude("error", kindErr, kindErr, usage)
		run.terminal(ev)
		return
	}
	outcome := "success"
	if ev.FinishReason == FinishCancelled {
		outcome = "cancelled"
	}
	run.conclude(outcome, nil, nil, usage)
	run.terminal(ev)
}

// failed ends the stream with an ERROR event (no vendor switch).
func (run *llmRun) failed(err *Error) {
	if err.Provider == "" {
		err.Provider = run.provider
	}
	if err.Capability == "" {
		err.Capability = CapLLM
	}
	run.conclude("error", err, err, run.estimate())
	run.terminal(LLMEvent{Type: LLMError, Err: err})
}

func (run *llmRun) admitFailed(err *Error) {
	if err.VendorCode != "circuit_open" { // admit already counted circuit_open
		run.g.r.metrics.ObserveCall(run.provider, CapLLM, run.req.Model, "", "error", err.Kind, err.VendorCode)
	}
	run.g.update(func(s *GenerationSummary) { s.LLMUsage = run.estimate() })
	run.terminal(LLMEvent{Type: LLMError, Err: err})
}

func (run *llmRun) cancelled() {
	usage := run.estimate()
	run.conclude("cancelled", nil, context.Canceled, usage)
	run.terminal(LLMEvent{Type: LLMDone, FinishReason: FinishCancelled, Usage: &usage})
}

// contextEnded handles the attempt context ending: caller cancel, the session deadline or
// LLM_TOTAL. After MarkAudioSent a deadline is a truncated answer, not a failure (S-7).
func (run *llmRun) contextEnded(parent, attempt context.Context) {
	sessionDeadline := errors.Is(parent.Err(), context.DeadlineExceeded)
	llmTotal := !sessionDeadline && parent.Err() == nil && errors.Is(attempt.Err(), context.DeadlineExceeded)
	if !sessionDeadline && !llmTotal {
		run.cancelled()
		return
	}
	timeout := ctxError(run.provider, CapLLM, context.DeadlineExceeded)
	if run.g.audioSent.Load() {
		usage := run.estimate()
		var ticketErr error
		if llmTotal {
			ticketErr = timeout // the session deadline is not the vendor's fault
		}
		run.conclude("truncated", nil, ticketErr, usage)
		run.terminal(LLMEvent{Type: LLMDone, FinishReason: FinishTruncated, Usage: &usage})
		return
	}
	run.failed(timeout)
}
