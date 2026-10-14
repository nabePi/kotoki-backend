package provider

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

const (
	minSpeakingRate  = 0.80
	maxSpeakingRate  = 1.05
	fakeTTSModel     = "fake-tts"
	fakeTTSVoiceID   = "FAKE"
	vendorCodeSwitch = "tts_provider_switched"
	vendorCodeVoice  = "voice_binding_missing"
)

// ttsOut is a router-wrapped TTS stream of a generation.
type ttsOut struct {
	ch     chan TTSChunk
	cancel context.CancelFunc
	epoch  int
	silent atomic.Bool // closed by the caller or superseded by an epoch switch: end without a chunk
	once   sync.Once
}

func (o *ttsOut) Chunks() <-chan TTSChunk { return o.ch }

func (o *ttsOut) Close() error {
	o.once.Do(func() {
		o.silent.Store(true)
		o.cancel()
	})
	return nil
}

func (g *generation) fakeTTS() bool { return g.r.cfg.ModeFor(CapTTS) == ModeFake }

// slot describes the voice slot a sentence is synthesised with.
type slot struct {
	onFallback bool
	vendor     Name
	ref        VoiceRef
	epoch      int
}

// slotFor resolves the vendor and voice of the primary or fallback slot. In fake mode the
// character's voice is ignored and the vendor follows the slot (FAKE_PRIMARY/FAKE_FALLBACK).
func (g *generation) slotFor(onFallback bool, epoch int) slot {
	sl := slot{onFallback: onFallback, epoch: epoch, ref: g.voice.Primary}
	if onFallback && g.voice.Fallback != nil {
		sl.ref = *g.voice.Fallback
	}
	sl.vendor = sl.ref.Provider
	if g.fakeTTS() {
		sl.vendor = FakePrimary
		if onFallback {
			sl.vendor = FakeFallback
		}
	}
	return sl
}

func (g *generation) fallbackUsable() bool {
	return g.voice.Fallback != nil && g.r.cfg.TTSFallbackEnabled
}

func (g *generation) registered(vendor Name) bool {
	_, ok := g.r.tts[vendor]
	return ok
}

// pinLocked returns the slot for a new sentence (g.mu held). The first call fixes the initial
// slot: the character's fallback when the primary vendor is unusable (breaker open or vendor
// not enabled) and the fallback vendor is registered.
func (g *generation) pinLocked() slot {
	if !g.ttsInit {
		g.ttsInit = true
		primary := g.slotFor(false, 0)
		fallback := g.slotFor(true, 0)
		if g.fallbackUsable() && g.registered(fallback.vendor) {
			reason := ""
			switch {
			case !g.registered(primary.vendor):
				reason = vendorCodeVoice
			case !g.r.breakers[key{primary.vendor, CapTTS}].WouldAllow():
				reason = "CIRCUIT_OPEN"
			}
			if reason != "" {
				g.ttsOnFallback = true
				g.summary.UsedFallback = true
				g.r.health.Fallback(primary.vendor, CapTTS)
				g.r.metrics.ObserveFallback(primary.vendor, fallback.vendor, reason)
			}
		}
	}
	return g.slotFor(g.ttsOnFallback, g.ttsEpoch)
}

type switchResult int

const (
	switchDone    switchResult = iota // this call moved the generation to the fallback voice
	switchAlready                     // another sentence already did
	switchRefused                     // audio was sent: the voice is frozen
)

// switchTTS moves the generation to the character's fallback voice once, bumping the epoch
// and cancelling the streams of the old epoch. It refuses once audio was sent (MarkAudioSent
// takes the same lock, so the check cannot race the pacer).
func (g *generation) switchTTS(from slot, reason ErrorKind) switchResult {
	g.mu.Lock()
	if g.audioSent.Load() {
		g.mu.Unlock()
		return switchRefused
	}
	if g.ttsOnFallback {
		g.mu.Unlock()
		return switchAlready
	}
	g.ttsOnFallback = true
	g.ttsEpoch++
	epoch := g.ttsEpoch
	g.summary.UsedFallback = true
	var stale []*ttsOut
	for o := range g.ttsStreams {
		if o.epoch < epoch {
			stale = append(stale, o)
		}
	}
	to := g.slotFor(true, epoch)
	g.mu.Unlock()

	for _, o := range stale {
		o.silent.Store(true)
		o.cancel()
	}
	select {
	case g.epochCh <- epoch:
	default:
	}
	g.r.health.Fallback(from.vendor, CapTTS)
	g.r.metrics.ObserveFallback(from.vendor, to.vendor, string(reason))
	g.r.metrics.TTSEpochRestart(from.vendor, to.vendor)
	g.r.log.Warn("provider tts fallback", "from", string(from.vendor), "to", string(to.vendor), "reason", string(reason))
	return switchDone
}

// Synthesize speaks one sentence with the generation's pinned voice.
func (g *generation) Synthesize(ctx context.Context, req TTSRequest) (TTSStream, error) {
	switch {
	case req.SpeakingRate == 0:
		req.SpeakingRate = 1.0
	case req.SpeakingRate < minSpeakingRate || req.SpeakingRate > maxSpeakingRate:
		return nil, NewError(g.voice.Primary.Provider, CapTTS, KindInvalidRequest, "SpeakingRate must be within 0.80–1.05")
	}
	sctx, cancel := context.WithCancel(ctx)
	// Choosing the voice and registering the stream are one critical section: a stream bound to
	// the old epoch is always visible to the switch that cancels it.
	g.mu.Lock()
	sl := g.pinLocked()
	out := &ttsOut{ch: make(chan TTSChunk, 32), cancel: cancel, epoch: sl.epoch}
	g.ttsStreams[out] = struct{}{}
	g.mu.Unlock()
	go g.runTTS(ctx, sctx, out, sl, req)
	return out, nil
}

// ttsRun carries the bookkeeping of one sentence attempt.
type ttsRun struct {
	parent   context.Context
	g        *generation
	out      *ttsOut
	sl       slot
	req      TTSRequest
	model    string
	voiceID  string
	start    time.Time
	sawChunk bool
	firstMs  int64
	ticket   Ticket
	release  func()
}

func (g *generation) runTTS(parent, sctx context.Context, out *ttsOut, sl slot, req TTSRequest) {
	r := g.r
	defer func() {
		g.mu.Lock()
		delete(g.ttsStreams, out)
		g.mu.Unlock()
		close(out.ch)
		out.cancel()
	}()
	run := &ttsRun{g: g, out: out, sl: sl, req: req, start: r.clock.Now(), parent: parent}

	// voice binding (spec 8.4.12): fake mode ignores the character's voice but its slot's
	// adapter must still be registered
	run.model, run.voiceID = fakeTTSModel, fakeTTSVoiceID
	if !g.fakeTTS() {
		run.model, run.voiceID = sl.ref.ModelName, sl.ref.VoiceID
	}
	if !g.registered(sl.vendor) || (!g.fakeTTS() && sl.ref.VoiceID == "") {
		e := NewError(sl.vendor, CapTTS, KindInvalidRequest, "voice binding is missing or its vendor is not enabled")
		e.VendorCode = vendorCodeVoice
		run.failBeforeAdapter(e) // a configuration problem: never counted by the breaker
		return
	}
	tk, rel, aerr := r.admitVendor(sctx, sl.vendor, CapTTS)
	if aerr != nil {
		if aerr.Kind == KindCancelled {
			return
		}
		run.failBeforeAdapter(aerr)
		return
	}
	run.ticket, run.release = tk, rel
	req.VoiceID, req.Model = run.voiceID, run.model

	attemptCtx, cancelAttempt := context.WithTimeout(sctx, r.cfg.TimeoutTTSSentenceTotal)
	defer cancelAttempt()
	g.update(func(s *GenerationSummary) { s.TTSCharacters += utf8.RuneCountInString(req.Text) })
	stream, err := r.tts[sl.vendor].Synthesize(attemptCtx, req)
	if err != nil {
		run.fail(asError(sl.vendor, CapTTS, err))
		return
	}
	defer func() { _ = stream.Close() }()

	firstTimer := time.NewTimer(r.cfg.TimeoutTTSFirstAudio)
	defer firstTimer.Stop()
	firstC := firstTimer.C
	for {
		select {
		case c, ok := <-stream.Chunks():
			if !ok {
				if out.silent.Load() || sctx.Err() != nil {
					run.settleCancelled()
					return
				}
				run.fail(NewError(sl.vendor, CapTTS, KindUnavailable, "stream ended without a final chunk"))
				return
			}
			if c.Err != nil {
				run.fail(asError(sl.vendor, CapTTS, c.Err))
				return
			}
			if !run.sawChunk {
				run.sawChunk = true
				firstTimer.Stop()
				firstC = nil
				run.firstMs = r.clock.Now().Sub(run.start).Milliseconds()
				g.update(func(s *GenerationSummary) {
					s.TTSProvider, s.TTSModel, s.TTSVoiceID = sl.vendor, run.model, run.voiceID
					if s.TTSFirstAudioMs == 0 {
						s.TTSFirstAudioMs = run.firstMs
					}
				})
			}
			c.Epoch = out.epoch
			if !run.forward(sctx, c) {
				run.settleCancelled()
				return
			}
			if c.Final {
				run.settleSuccess(c)
				return
			}
		case <-firstC:
			cancelAttempt()
			run.fail(&Error{Provider: sl.vendor, Capability: CapTTS, Kind: KindTimeout, VendorCode: "first_audio_timeout", Retryable: true,
				Message: "no audio within the budget"})
			return
		case <-attemptCtx.Done():
			switch {
			case out.silent.Load() || parent.Err() != nil || sctx.Err() != nil:
				run.settleCancelled()
			default:
				run.fail(ctxError(sl.vendor, CapTTS, attemptCtx.Err()))
			}
			return
		}
	}
}

func (run *ttsRun) forward(ctx context.Context, c TTSChunk) bool {
	c.Provider, c.Model, c.ReceivedAt = run.sl.vendor, run.model, run.g.r.clock.Now()
	select {
	case run.out.ch <- c:
		return true
	default:
	}
	select {
	case run.out.ch <- c:
		return true
	case <-ctx.Done():
		return false
	}
}

// settle releases the semaphore and completes the breaker ticket exactly once.
func (run *ttsRun) settle(ticketErr error) {
	if run.release != nil {
		run.ticket.Done(ticketErr)
		run.release()
		run.release = nil
	}
}

func (run *ttsRun) record(outcome string, err *Error) {
	g, r, sl := run.g, run.g.r, run.sl
	kind, code := ErrorKind(""), ""
	if err != nil {
		kind, code = err.Kind, err.VendorCode
	}
	r.metrics.ObserveCall(sl.vendor, CapTTS, run.model, "", outcome, kind, code)
	total := r.clock.Now().Sub(run.start)
	if run.sawChunk {
		r.metrics.ObserveLatency(sl.vendor, CapTTS, run.model, "first_audio", time.Duration(run.firstMs)*time.Millisecond)
	}
	r.metrics.ObserveLatency(sl.vendor, CapTTS, run.model, "total", total)
	var herr error
	if outcome == "error" && err != nil {
		herr = err
	} else if outcome == "cancelled" {
		herr = context.Canceled
	}
	r.health.Record(sl.vendor, CapTTS, herr, total)
	r.log.Debug("provider call", "provider", string(sl.vendor), "capability", string(CapTTS), "model", run.model, "request_id", run.req.RequestID,
		"session_id", g.sessionID.String(), "outcome", outcome, "kind", string(kind), "latency_first_ms", run.firstMs, "latency_total_ms", total.Milliseconds())
}

func (run *ttsRun) settleSuccess(final TTSChunk) {
	r, sl := run.g.r, run.sl
	run.settle(nil)
	run.record("success", nil)
	if final.Usage != nil {
		r.metrics.Usage(sl.vendor, CapTTS, run.model, "characters", float64(final.Usage.Characters))
		r.metrics.Usage(sl.vendor, CapTTS, run.model, "audio_ms", float64(final.Usage.AudioMs))
		if r.prices != nil {
			if usd, ok := r.prices.TTS(sl.vendor, run.model, final.Usage.Characters); ok {
				r.metrics.CostUSD(sl.vendor, CapTTS, run.model, usd)
			} else if usd, ok := r.prices.Audio(sl.vendor, CapTTS, run.model, final.Usage.AudioMs); ok {
				r.metrics.CostUSD(sl.vendor, CapTTS, run.model, usd)
			}
		}
	}
}

func (run *ttsRun) settleCancelled() {
	run.settle(context.Canceled)
	run.record("cancelled", nil)
}

// fail ends the sentence after the vendor was called: it settles the breaker with err and
// then applies the fallback rule.
func (run *ttsRun) fail(err *Error) {
	run.settle(err)
	run.record("error", err)
	run.emitFailure(err)
}

// failBeforeAdapter handles failures that never reached the vendor (binding, breaker,
// semaphore); admitVendor already settled the breaker where it applies.
func (run *ttsRun) failBeforeAdapter(err *Error) {
	if err.VendorCode != "circuit_open" {
		run.g.r.metrics.ObserveCall(run.sl.vendor, CapTTS, run.model, "", "error", err.Kind, err.VendorCode)
	}
	run.emitFailure(err)
}

// emitFailure applies the TTS fallback rule (spec 8.5.7): only before audio was sent, only
// for eligible errors, only once, only when the character has a fallback voice.
func (run *ttsRun) emitFailure(err *Error) {
	g := run.g
	if run.out.silent.Load() || errors.Is(run.parent.Err(), context.Canceled) {
		return // closed, superseded or cancelled by the caller: nobody is waiting for this
	}
	if err.Provider == "" {
		err.Provider = run.sl.vendor
	}
	callerGaveUp := run.parent.Err() != nil // its own deadline: not the vendor's fault, no voice change
	canSwitch := !callerGaveUp && FallbackEligible(err) && !run.sl.onFallback && g.fallbackUsable()
	if canSwitch && g.switchTTS(run.sl, err.Kind) != switchRefused {
		sw := &Error{Provider: run.sl.vendor, Capability: CapTTS, Kind: KindUnavailable, VendorCode: vendorCodeSwitch, Retryable: true,
			Message: "tts moved to the character's fallback voice", Cause: err}
		run.deliver(TTSChunk{Err: sw, Epoch: run.out.epoch})
		return
	}
	run.deliver(TTSChunk{Err: err, Epoch: run.out.epoch})
}

func (run *ttsRun) deliver(c TTSChunk) {
	c.Provider, c.ReceivedAt = run.sl.vendor, run.g.r.clock.Now()
	select {
	case run.out.ch <- c:
	case <-time.After(time.Second): // nobody is reading
	}
}
