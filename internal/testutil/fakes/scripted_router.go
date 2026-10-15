package fakes

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/google/uuid"

	"github.com/nabePi/kotoki-backend/internal/provider"
)

// vendorCodeTTSSwitched is the router's VendorCode for the error that ends a sentence when TTS
// moved to the character's fallback voice (provider/generation_tts.go).
const vendorCodeTTSSwitched = "tts_provider_switched"

// ttsBytesPerMs is PCM16LE mono 24 kHz.
const ttsBytesPerMs = 2 * provider.TTSSampleRate / 1000

// Script drives every generation of a ScriptedRouter.
type Script struct {
	// Deltas are the LLM DELTA texts, in order.
	Deltas []string
	// Finish is the DONE reason after the last delta ("" → STOP).
	Finish provider.FinishReason
	// LLMErrAfter > 0 (or LLMErr != nil): an ERROR event replaces the delta with this index
	// (after LLMErrAfter deltas); LLMErr nil → an UNAVAILABLE error.
	LLMErrAfter int
	LLMErr      *provider.Error
	// PCMPerSentenceMS is the audio of each sentence (0 → 100 ms = 5 frames), sent in two
	// chunks split on an odd byte, then a Final chunk.
	PCMPerSentenceMS int
	// Gate, when set, holds the last delta until it is closed.
	Gate <-chan struct{}
	// TTSEpochAfterSentence > 0: once sentence index TTSEpochAfterSentence has been synthesised
	// in epoch 0, the request of the next sentence sends 20 ms of epoch-0 audio and then fails;
	// like the router, a character with voice.fallback and no audio sent yet moves to epoch 1
	// (TTSEpochChanged, error tts_provider_switched), otherwise the error is UNAVAILABLE. The
	// first request of epoch 1 starts with one stale epoch-0 chunk that must be ignored.
	TTSEpochAfterSentence int
	// TTSErr, when set, fails the epoch-0 request of sentence TTSErrSentence before any audio;
	// with voice.fallback and no audio sent it moves to epoch 1 like the router.
	TTSErr         *provider.Error
	TTSErrSentence int
}

// ScriptedRouter is a deterministic provider.RealtimeRouter for the generation pipeline: every
// NewGeneration plays the Script. It records LLM/TTS requests and an ordered log per generation.
// Safe for concurrent use.
type ScriptedRouter struct {
	script Script

	mu     sync.Mutex
	gens   []*ScriptedGeneration
	onMark func()
}

var _ provider.RealtimeRouter = (*ScriptedRouter)(nil)

// NewScriptedRouter returns a router playing s.
func NewScriptedRouter(s Script) *ScriptedRouter { return &ScriptedRouter{script: s} }

// SetOnMarkAudioSent installs a hook called (outside any lock) from every MarkAudioSent.
func (r *ScriptedRouter) SetOnMarkAudioSent(f func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.onMark = f
}

// OpenSTT is not scripted.
func (r *ScriptedRouter) OpenSTT(context.Context, provider.STTSessionConfig) (provider.RoutedSTTSession, error) {
	return nil, errors.New("fakes: ScriptedRouter has no STT")
}

// CompleteText is not scripted.
func (r *ScriptedRouter) CompleteText(context.Context, provider.TextRequest) (provider.TextResult, error) {
	return provider.TextResult{}, errors.New("fakes: ScriptedRouter has no CompleteText")
}

// NewGeneration implements provider.RealtimeRouter.
func (r *ScriptedRouter) NewGeneration(sessionID uuid.UUID, gen int64, voice provider.VoiceSelection) provider.Generation {
	g := &ScriptedGeneration{SessionID: sessionID, Gen: gen, Voice: voice, r: r, epochCh: make(chan int, 1)}
	r.mu.Lock()
	r.gens = append(r.gens, g)
	r.mu.Unlock()
	return g
}

// Generations returns the generations created so far, in order.
func (r *ScriptedRouter) Generations() []*ScriptedGeneration {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]*ScriptedGeneration(nil), r.gens...)
}

// TTSCall is one Synthesize request; Index comes from RequestID, Epoch is the epoch it was bound to.
type TTSCall struct {
	Req   provider.TTSRequest
	Index int
	Epoch int
}

// ScriptedGeneration is the provider.Generation of a ScriptedRouter.
type ScriptedGeneration struct {
	SessionID uuid.UUID
	Gen       int64
	Voice     provider.VoiceSelection

	r       *ScriptedRouter
	epochCh chan int

	mu           sync.Mutex
	llmReqs      []provider.LLMRequest
	ttsCalls     []TTSCall
	log          []string
	marks        int
	audioSent    bool
	epoch        int
	switched     bool // the scripted failure already happened
	staleSent    bool // the stale chunk of the new epoch was sent
	llmCancelled bool
}

var _ provider.Generation = (*ScriptedGeneration)(nil)

func (g *ScriptedGeneration) logf(format string, a ...any) {
	g.mu.Lock()
	g.log = append(g.log, fmt.Sprintf(format, a...))
	g.mu.Unlock()
}

// Log returns the ordered call log: "llm:start", "llm:delta:<i>", "llm:done:<reason>",
// "llm:error", "tts:<index>:e<epoch>", "tts:final:<index>:e<epoch>", "mark".
func (g *ScriptedGeneration) Log() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.log...)
}

// LLMRequests returns the StreamLLM requests.
func (g *ScriptedGeneration) LLMRequests() []provider.LLMRequest {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]provider.LLMRequest(nil), g.llmReqs...)
}

// TTSCalls returns the Synthesize requests, in order.
func (g *ScriptedGeneration) TTSCalls() []TTSCall {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]TTSCall(nil), g.ttsCalls...)
}

// MarkAudioSentCalls counts MarkAudioSent.
func (g *ScriptedGeneration) MarkAudioSentCalls() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.marks
}

// Epoch is the current TTS epoch.
func (g *ScriptedGeneration) Epoch() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.epoch
}

// LLMCancelled reports whether the ctx given to StreamLLM was cancelled.
func (g *ScriptedGeneration) LLMCancelled() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.llmCancelled
}

// MarkAudioSent implements provider.Generation.
func (g *ScriptedGeneration) MarkAudioSent() {
	g.mu.Lock()
	g.marks++
	g.audioSent = true
	g.log = append(g.log, "mark")
	g.mu.Unlock()
	g.r.mu.Lock()
	hook := g.r.onMark
	g.r.mu.Unlock()
	if hook != nil {
		hook()
	}
}

// ForceEpoch moves TTS to epoch n at once and announces it on TTSEpochChanged, whether or not
// audio was sent: it stands in for a router switch that raced MarkAudioSent. Later requests are
// bound to epoch n (the first one starts with a stale epoch-0 chunk).
func (g *ScriptedGeneration) ForceEpoch(n int) {
	g.mu.Lock()
	g.epoch, g.switched = n, true
	g.log = append(g.log, fmt.Sprintf("force:e%d", n))
	g.mu.Unlock()
	select {
	case g.epochCh <- n:
	default:
	}
}

// TTSEpochChanged implements provider.Generation.
func (g *ScriptedGeneration) TTSEpochChanged() <-chan int { return g.epochCh }

// Summary implements provider.Generation.
func (g *ScriptedGeneration) Summary() provider.GenerationSummary {
	g.mu.Lock()
	defer g.mu.Unlock()
	s := provider.GenerationSummary{LLMProvider: provider.FakePrimary, LLMModel: "fake-llm", UsedFallback: g.epoch > 0}
	for _, c := range g.ttsCalls {
		s.TTSCharacters += len([]rune(c.Req.Text))
	}
	if g.audioSent {
		s.TTSProvider, s.TTSModel, s.TTSVoiceID = g.Voice.Primary.Provider, g.Voice.Primary.ModelName, g.Voice.Primary.VoiceID
		if g.epoch > 0 && g.Voice.Fallback != nil {
			s.TTSProvider, s.TTSModel, s.TTSVoiceID = g.Voice.Fallback.Provider, g.Voice.Fallback.ModelName, g.Voice.Fallback.VoiceID
		}
	}
	return s
}

// scriptedLLM is the LLMStream of a ScriptedGeneration.
type scriptedLLM struct {
	ch     chan provider.LLMEvent
	cancel context.CancelFunc
}

func (s *scriptedLLM) Events() <-chan provider.LLMEvent { return s.ch }
func (s *scriptedLLM) Close() error                     { s.cancel(); return nil }

// StreamLLM implements provider.Generation: it plays Script.Deltas on a goroutine that ends on
// ctx (DONE{CANCELLED}, like the router) or after the terminal event.
func (g *ScriptedGeneration) StreamLLM(ctx context.Context, req provider.LLMRequest) (provider.LLMStream, error) {
	g.mu.Lock()
	g.llmReqs = append(g.llmReqs, req)
	g.log = append(g.log, "llm:start")
	g.mu.Unlock()
	context.AfterFunc(ctx, func() {
		g.mu.Lock()
		g.llmCancelled = true
		g.mu.Unlock()
	})
	sctx, cancel := context.WithCancel(ctx)
	s := &scriptedLLM{ch: make(chan provider.LLMEvent, 128), cancel: cancel}
	go g.playLLM(sctx, s.ch)
	return s, nil
}

func (g *ScriptedGeneration) playLLM(ctx context.Context, out chan<- provider.LLMEvent) {
	defer close(out)
	sc := g.r.script
	send := func(ev provider.LLMEvent) bool {
		select {
		case out <- ev:
			return true
		case <-ctx.Done():
			return false
		}
	}
	cancelled := func() {
		select { // best effort, like the router's terminal event
		case out <- provider.LLMEvent{Type: provider.LLMDone, FinishReason: provider.FinishCancelled, Usage: &provider.TokenUsage{}}:
		default:
		}
	}
	failAt := -1
	if sc.LLMErrAfter > 0 || sc.LLMErr != nil {
		failAt = sc.LLMErrAfter
	}
	llmErr := sc.LLMErr
	if llmErr == nil {
		llmErr = provider.NewError(provider.FakePrimary, provider.CapLLM, provider.KindUnavailable, "scripted LLM failure")
	}
	for i, d := range sc.Deltas {
		if i == failAt {
			g.logf("llm:error")
			send(provider.LLMEvent{Type: provider.LLMError, Err: llmErr})
			return
		}
		if i == len(sc.Deltas)-1 && sc.Gate != nil {
			select {
			case <-sc.Gate:
			case <-ctx.Done():
				cancelled()
				return
			}
		}
		if ctx.Err() != nil || !send(provider.LLMEvent{Type: provider.LLMDelta, Text: d, Provider: provider.FakePrimary}) {
			cancelled()
			return
		}
		g.logf("llm:delta:%d", i)
	}
	if failAt >= len(sc.Deltas) {
		g.logf("llm:error")
		send(provider.LLMEvent{Type: provider.LLMError, Err: llmErr})
		return
	}
	finish := sc.Finish
	if finish == "" {
		finish = provider.FinishStop
	}
	g.logf("llm:done:%s", finish)
	send(provider.LLMEvent{Type: provider.LLMDone, FinishReason: finish, Usage: &provider.TokenUsage{}, Provider: provider.FakePrimary})
}

// scriptedTTS is the TTSStream of a ScriptedGeneration.
type scriptedTTS struct {
	ch     chan provider.TTSChunk
	cancel context.CancelFunc
}

func (s *scriptedTTS) Chunks() <-chan provider.TTSChunk { return s.ch }
func (s *scriptedTTS) Close() error                     { s.cancel(); return nil }

// ttsPlan is what one Synthesize request does.
type ttsPlan struct {
	index, epoch int
	stale        bool            // start with one stale epoch-0 chunk
	fail         *provider.Error // fail (after preAudio) instead of speaking
	preAudio     bool            // send 20 ms before failing
	switchTo     int             // > 0: move to this epoch before the failure
}

func sentenceIndex(requestID string) int {
	i := strings.LastIndexByte(requestID, ':')
	n, err := strconv.Atoi(requestID[i+1:])
	if err != nil {
		return -1
	}
	return n
}

// Synthesize implements provider.Generation. VoiceID/Model are left as the runtime sent them.
func (g *ScriptedGeneration) Synthesize(ctx context.Context, req provider.TTSRequest) (provider.TTSStream, error) {
	sc := g.r.script
	idx := sentenceIndex(req.RequestID)
	g.mu.Lock()
	p := ttsPlan{index: idx, epoch: g.epoch}
	g.ttsCalls = append(g.ttsCalls, TTSCall{Req: req, Index: idx, Epoch: g.epoch})
	g.log = append(g.log, fmt.Sprintf("tts:%d:e%d", idx, g.epoch))
	if g.epoch > 0 && !g.staleSent {
		g.staleSent, p.stale = true, true
	}
	if g.epoch == 0 && !g.switched {
		var cause *provider.Error
		switch {
		case sc.TTSErr != nil && idx == sc.TTSErrSentence:
			cause = sc.TTSErr
		case sc.TTSEpochAfterSentence > 0 && idx == sc.TTSEpochAfterSentence+1:
			cause, p.preAudio = provider.NewError(g.Voice.Primary.Provider, provider.CapTTS, provider.KindUnavailable, "scripted TTS failure"), true
		}
		if cause != nil {
			g.switched = true
			p.fail = cause
			if g.Voice.Fallback != nil && !g.audioSent && provider.FallbackEligible(cause) {
				g.epoch++
				p.switchTo = g.epoch
				p.fail = &provider.Error{Provider: cause.Provider, Capability: provider.CapTTS, Kind: provider.KindUnavailable,
					VendorCode: vendorCodeTTSSwitched, Retryable: true, Message: "tts moved to the character's fallback voice", Cause: cause}
			}
		}
	}
	g.mu.Unlock()
	sctx, cancel := context.WithCancel(ctx)
	s := &scriptedTTS{ch: make(chan provider.TTSChunk, 32), cancel: cancel}
	go g.playTTS(sctx, s.ch, p)
	return s, nil
}

func (g *ScriptedGeneration) playTTS(ctx context.Context, out chan<- provider.TTSChunk, p ttsPlan) {
	defer close(out)
	send := func(c provider.TTSChunk) bool {
		select {
		case out <- c:
			return true
		case <-ctx.Done():
			return false
		}
	}
	ms := g.r.script.PCMPerSentenceMS
	if ms <= 0 {
		ms = 100
	}
	if p.stale && !send(provider.TTSChunk{PCM: make([]byte, 20*ttsBytesPerMs), Epoch: 0, Provider: provider.FakePrimary}) {
		return
	}
	if p.fail != nil {
		if p.preAudio && !send(provider.TTSChunk{PCM: make([]byte, 20*ttsBytesPerMs), Epoch: p.epoch, Provider: provider.FakePrimary}) {
			return
		}
		if p.switchTo > 0 {
			g.epochCh <- p.switchTo // buffered 1, one switch per generation
		}
		send(provider.TTSChunk{Err: p.fail, Epoch: p.epoch, Provider: g.Voice.Primary.Provider})
		return
	}
	pcm := make([]byte, ms*ttsBytesPerMs)
	for i := range pcm {
		pcm[i] = byte(i * 7) // not silence: the encoder sees a waveform
	}
	half := len(pcm)/2 + 1 // odd split: the framer carries a byte
	for _, part := range [][]byte{pcm[:half], pcm[half:]} {
		if !send(provider.TTSChunk{PCM: part, Epoch: p.epoch, Provider: provider.FakePrimary}) {
			return
		}
	}
	if send(provider.TTSChunk{Final: true, Epoch: p.epoch, Provider: provider.FakePrimary,
		Usage: &provider.TTSUsage{Characters: 1, AudioMs: int64(ms)}}) {
		g.logf("tts:final:%d:e%d", p.index, p.epoch)
	}
}
