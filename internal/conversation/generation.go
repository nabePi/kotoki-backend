package conversation

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/nabePi/kotoki-backend/internal/content"
	"github.com/nabePi/kotoki-backend/internal/conversation/audio"
	"github.com/nabePi/kotoki-backend/internal/platform/clock"
	"github.com/nabePi/kotoki-backend/internal/prompt"
	"github.com/nabePi/kotoki-backend/internal/provider"
)

// GenKind is the kind of an AI generation (ai.generation_started.kind).
type GenKind string

// Generation kinds.
const (
	GenOpening GenKind = "OPENING"
	GenReply   GenKind = "REPLY"
	GenApology GenKind = "APOLOGY"
)

// metricLabel is the kind label of ai_sentence_limit_total (`reply`, `opening`).
func (k GenKind) metricLabel() string { return strings.ToLower(string(k)) }

// GenCause is why a generation failed (GENERATION_FAILED, §7.6.1 rows 11/12/16).
type GenCause string

// Failure causes. CauseContentFiltered is S-9 (DONE{CONTENT_FILTERED} or ERROR kind
// CONTENT_FILTERED); it has its own counter rules in the loop.
const (
	CauseProvider        GenCause = "PROVIDER"
	CauseContentFiltered GenCause = "CONTENT_FILTERED"
)

const (
	genSentencesCap       = 8   // §7.7.2 sentences(8)
	genFramesCap          = 250 // §7.7.2 frames(250) = 5 s of audio
	defaultOpusBitrate    = 32000
	defaultOpusComplexity = 5
	defaultMaxLeadMS      = 400
	ttsLanguage           = "ja"
	// vendorCodeTTSSwitched is the router's VendorCode on the error that ends the sentence whose
	// failure moved TTS to the character's fallback voice (provider/generation_tts.go): not a
	// failure, the new epoch follows on TTSEpochChanged.
	vendorCodeTTSSwitched = "tts_provider_switched"
)

// GenEvent is what the goroutines of a generation send to Runtime.loop (genEvents, cap 128).
// Every event carries its generation; the loop drops events with Gen != r.gen.Number.
type GenEvent interface{ GenNumber() uint32 }

// EvTextDelta is sanitised new text (→ ai.text_delta and assistant_text).
type EvTextDelta struct {
	Gen   uint32
	Clean string
}

// EvSentence is a sentence cut by the parser (→ transcript; ai.sentence comes with its audio).
type EvSentence struct {
	Gen      uint32
	Sentence prompt.Sentence
}

// EvFirstAudio is FIRST_AUDIO: the first frame was queued for the writer after MarkAudioSent (S-12).
type EvFirstAudio struct{ Gen uint32 }

// EvSentenceAudio is the first frame of sentence Index queued (→ ai.sentence; start_ms =
// FirstFrameSeq × 20).
type EvSentenceAudio struct {
	Gen           uint32
	Index         int
	FirstFrameSeq uint32
}

// EvAudioDone is AUDIO_DONE: the writer wrote every frame of the generation (AUDIO_WRITTEN_ALL).
// LLMFinished is ai.audio_completed.llm_finished; FrameCount/TotalMS/SentenceCount fill the rest
// of that payload.
type EvAudioDone struct {
	Gen           uint32
	LLMFinished   bool
	FrameCount    int
	TotalMS       int64
	SentenceCount int
}

// EvGenFailed is GENERATION_FAILED.
type EvGenFailed struct {
	Gen   uint32
	Cause GenCause
}

// GenNumber implements GenEvent.
func (e EvTextDelta) GenNumber() uint32 { return e.Gen }

// GenNumber implements GenEvent.
func (e EvSentence) GenNumber() uint32 { return e.Gen }

// GenNumber implements GenEvent.
func (e EvFirstAudio) GenNumber() uint32 { return e.Gen }

// GenNumber implements GenEvent.
func (e EvSentenceAudio) GenNumber() uint32 { return e.Gen }

// GenNumber implements GenEvent.
func (e EvAudioDone) GenNumber() uint32 { return e.Gen }

// GenNumber implements GenEvent.
func (e EvGenFailed) GenNumber() uint32 { return e.Gen }

// genParams is everything one generation needs; Runtime.startGeneration (Task 19) fills it.
type genParams struct {
	Number    uint32
	Kind      GenKind
	TurnID    uuid.UUID
	SessionID uuid.UUID
	// Parent is the session context; Deadline (zero: none) is expires_at + PROVIDER_SESSION_GRACE.
	Parent   context.Context
	Deadline time.Time

	// Router is used for OPENING/REPLY only (APOLOGY plays a pre-rendered asset).
	Router provider.RealtimeRouter
	// Voice is voiceSelection(snapshot character), computed once per session (D-015).
	Voice provider.VoiceSelection
	// VoiceSpeakingRate is config_snapshot.character.voice.speaking_rate; AdaptiveRate is
	// adaptive.TTSSpeakingRate. The TTS rate is computed from them once per generation.
	VoiceSpeakingRate float64
	AdaptiveRate      float64
	// MaxSentences is adaptive.MaxAISentencesPerTurn at generation start (R-17; clamp ≥ 1).
	MaxSentences int
	// LLM carries Messages, MaxTokens and Temperature of the built prompt; RequestID is set here.
	LLM provider.LLMRequest

	Out    *Outbound
	Events chan<- GenEvent // genEvents (128)

	Clock          clock.Clock                                      // nil → system clock
	Sleep          func(ctx context.Context, d time.Duration) error // pacer wait; nil → sleepCtx
	MaxLeadMS      int64                                            // AUDIO_MAX_LEAD_MS; 0 → 400
	OpusBitrate    int                                              // OPUS_BITRATE; 0 → 32000
	OpusComplexity int                                              // OPUS_COMPLEXITY; 0 → 5
	Metrics        *Metrics
	// Asset returns the APOLOGY packets and AssetText their text; nil/"" → the embedded clip.
	Asset     func() ([][]byte, error)
	AssetText string
	// OnPanic, when set, receives the value of a panic in any goroutine of the generation (which is
	// recovered): the runtime turns it into FATAL{SERVER_ERROR} (§7.7.2, §7.11.4). Nil: the panic
	// propagates, as it does in a test that wants to see it.
	OnPanic func(v any)
}

// Generation is one AI generation (§7.7.4): runLLM (or runStatic) → sentences(8) → runTTS →
// frames(250) → pacer → Outbound; APOLOGY is playAsset → frames → pacer. ctx/cancel and
// llmCtx/llmCancel are owned by Runtime.loop, except that runLLM cuts its own stream at the R-17
// limit (§7.7.3) and a failed generation stops its LLM. Every goroutine ends on ctx; every
// producer closes its channel on every exit.
type Generation struct {
	Number uint32
	Kind   GenKind
	TurnID uuid.UUID

	ctx       context.Context
	cancel    context.CancelFunc
	llmCtx    context.Context
	llmCancel context.CancelFunc

	p            genParams
	pg           provider.Generation // nil for APOLOGY
	speakingRate float64
	pacer        *Pacer
	wg           sync.WaitGroup

	// ttsStopped is closed when runTTS exits, so the text stage never blocks on a stage that is gone.
	ttsStopped chan struct{}

	llmFinished  atomic.Bool // the text is complete: STOP/MAX_TOKENS, R-17 limit, static, asset
	failed       atomic.Bool // EvGenFailed was decided (once)
	failedAt     atomic.Pointer[failInfo]
	audioDone    atomic.Bool // EvAudioDone was produced (once)
	framesQueued atomic.Int64

	// sentMu guards sentText: the text of each sentence the parser cut, recorded before the sentence
	// can be spoken. assistant_spoken_text is built from it (the writer's count says how many
	// sentences were heard), never from what Runtime.loop has had time to read from genEvents.
	sentMu   sync.Mutex
	sentText map[int]string
	fullText strings.Builder // every clean delta, in order: assistant_text

	// epochMu orders the pacer's admit decision against runTTS moving to a new TTS epoch.
	epochMu       sync.Mutex
	ttsEpoch      int  // frames of older epochs are dropped
	firstAdmitted bool // a frame was admitted (it is about to be queued after MarkAudioSent)
}

// newGeneration creates the generation (and its provider.Generation) without starting it.
func newGeneration(p genParams) *Generation {
	if p.Parent == nil {
		p.Parent = context.Background()
	}
	if p.Clock == nil {
		p.Clock = clock.System{}
	}
	if p.Sleep == nil {
		p.Sleep = sleepCtx
	}
	if p.MaxLeadMS <= 0 {
		p.MaxLeadMS = defaultMaxLeadMS
	}
	if p.OpusBitrate <= 0 {
		p.OpusBitrate = defaultOpusBitrate
	}
	if p.OpusComplexity <= 0 {
		p.OpusComplexity = defaultOpusComplexity
	}
	if p.Asset == nil {
		p.Asset = audio.ApologyFrames
	}
	if p.AssetText == "" {
		p.AssetText = audio.ApologyText
	}
	g := &Generation{Number: p.Number, Kind: p.Kind, TurnID: p.TurnID, p: p}
	if p.Deadline.IsZero() {
		g.ctx, g.cancel = context.WithCancel(p.Parent)
	} else {
		g.ctx, g.cancel = context.WithDeadline(p.Parent, p.Deadline)
	}
	g.llmCtx, g.llmCancel = context.WithCancel(g.ctx)
	if p.Kind != GenApology {
		if p.Router == nil {
			panic("conversation: a non-APOLOGY generation needs a router")
		}
		// every LLM/TTS call of the generation goes through provider.Generation (D-042)
		g.pg = p.Router.NewGeneration(p.SessionID, int64(p.Number), p.Voice)
		g.speakingRate = content.SpeakingRate(p.VoiceSpeakingRate, p.AdaptiveRate) // D-014, once per generation
	}
	g.pacer = NewPacer(p.Clock, p.Sleep, p.MaxLeadMS)
	return g
}

// voiceSelection is the session's TTS voice pair from the character snapshot (D-015): values are
// passed as they are; no voice.fallback → Fallback nil (TTS failure → generation FAILED, D-042).
func voiceSelection(c content.CharacterSnapshot) provider.VoiceSelection {
	pb := c.PrimaryTTS()
	sel := provider.VoiceSelection{Primary: provider.VoiceRef{Provider: provider.Name(pb.ProviderName), ModelName: pb.ModelName, VoiceID: pb.VoiceID}}
	if fb, ok := c.FallbackTTS(); ok {
		sel.Fallback = &provider.VoiceRef{Provider: provider.Name(fb.ProviderName), ModelName: fb.ModelName, VoiceID: fb.VoiceID}
	}
	return sel
}

// sleepCtx waits d or until ctx ends (the production pacer wait; tests inject a fake-clock one).
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Cancel stops the generation (barge-in, failure handling, session end).
func (g *Generation) Cancel() { g.cancel() }

// LLMCancel cuts only the LLM stream (expiry): queued sentences are still spoken.
func (g *Generation) LLMCancel() { g.llmCancel() }

// Wait blocks until every goroutine of the generation has exited.
func (g *Generation) Wait() { g.wg.Wait() }

// PlaybackEnd is the pacer's estimated end of client playback (zero before the first frame).
func (g *Generation) PlaybackEnd() time.Time { return g.pacer.EstimatedPlaybackEnd() }

// Summary is the provider summary for the turn columns (zero for APOLOGY).
func (g *Generation) Summary() provider.GenerationSummary {
	if g.pg == nil {
		return provider.GenerationSummary{}
	}
	return g.pg.Summary()
}

// OnWriterAck turns the writer's AUDIO_WRITTEN_ALL ack of this generation into AUDIO_DONE
// (§7.7.1 step 11): the pacer only queues the end marker after its last frame, so the ack means
// every frame was written. Called by Runtime.loop for each writer ack; true at most once, and
// never for a failed generation.
func (g *Generation) OnWriterAck(a WriterAck) (EvAudioDone, bool) {
	if a.Kind != AckAudioWrittenAll || a.Gen != g.Number || g.failed.Load() || !g.audioDone.CompareAndSwap(false, true) {
		return EvAudioDone{}, false
	}
	n := g.framesQueued.Load()
	return EvAudioDone{Gen: g.Number, LLMFinished: g.llmFinished.Load(), FrameCount: int(n),
		TotalMS: n * pacerFrameMS, SentenceCount: a.Sentences}, true
}

// recordSentence keeps the text of sentence s (see sentText).
func (g *Generation) recordSentence(s prompt.Sentence) {
	g.sentMu.Lock()
	defer g.sentMu.Unlock()
	if g.sentText == nil {
		g.sentText = map[int]string{}
	}
	g.sentText[s.Index] = s.Text
}

// recordText keeps the clean text of a delta (see fullText).
func (g *Generation) recordText(clean string) {
	g.sentMu.Lock()
	defer g.sentMu.Unlock()
	g.fullText.WriteString(clean)
}

// Text is everything the LLM produced so far, as the parser cleaned it (assistant_text). Safe for
// concurrent use; unlike what the loop has read from genEvents it is never behind the sentences.
func (g *Generation) Text() string {
	g.sentMu.Lock()
	defer g.sentMu.Unlock()
	return g.fullText.String()
}

// SentenceText is the text of sentence i ("" when it was never cut). Safe for concurrent use.
func (g *Generation) SentenceText(i int) string {
	g.sentMu.Lock()
	defer g.sentMu.Unlock()
	return g.sentText[i]
}

// startLLM runs an OPENING/REPLY generation from the LLM.
func (g *Generation) startLLM() { g.startText(g.runLLM) }

// startStatic runs the static OPENING (S-2): opening_line instead of the LLM.
func (g *Generation) startStatic(openingLine string) {
	g.startText(func(s chan<- prompt.Sentence) { g.runStatic(openingLine, s) })
}

func (g *Generation) startText(text func(chan<- prompt.Sentence)) {
	sentences := make(chan prompt.Sentence, genSentencesCap)
	frames := make(chan audio.OutFrame, genFramesCap)
	g.ttsStopped = make(chan struct{})
	g.spawn(func() { text(sentences) }, func() { g.runTTS(sentences, frames) }, func() { g.runPacer(frames) })
}

// startAsset runs an APOLOGY generation.
func (g *Generation) startAsset() {
	frames := make(chan audio.OutFrame, genFramesCap)
	g.spawn(func() { g.playAsset(frames) }, func() { g.runPacer(frames) })
}

func (g *Generation) spawn(fns ...func()) {
	g.wg.Add(len(fns))
	for _, f := range fns {
		go func() {
			defer g.wg.Done()
			if g.p.OnPanic != nil {
				defer func() {
					if v := recover(); v != nil {
						g.cancel() // the other goroutines stop and send nothing more: the fatal error is the only outcome
						g.p.OnPanic(v)
					}
				}()
			}
			f()
		}()
	}
}

// emit sends ev to the loop; false when the generation was cancelled.
func (g *Generation) emit(ev GenEvent) bool {
	if g.ctx.Err() != nil {
		return false
	}
	select {
	case g.p.Events <- ev:
		return true
	case <-g.ctx.Done():
		return false
	}
}

// Stages of a failed generation (TURN_FAILED.stage, §7.13.1).
const (
	failStageLLM = "LLM"
	failStageTTS = "TTS"
)

// failInfo is where a generation failed and the class of the error (TURN_FAILED.stage and
// error_class).
type failInfo struct{ stage, class string }

// FailInfo is the stage ("LLM" or "TTS") and error class of the failure the generation reported;
// empty when it did not fail. It is set before EvGenFailed is sent.
func (g *Generation) FailInfo() (stage, class string) {
	if fi := g.failedAt.Load(); fi != nil {
		return fi.stage, fi.class
	}
	return "", ""
}

// fail reports GENERATION_FAILED once and stops the LLM (no more tokens are useful). A cancelled
// generation never fails: cancellation belongs to the loop.
func (g *Generation) fail(c GenCause, stage, class string) {
	if g.ctx.Err() != nil || !g.failed.CompareAndSwap(false, true) {
		return
	}
	g.failedAt.Store(&failInfo{stage: stage, class: class})
	g.llmCancel()
	g.emit(EvGenFailed{Gen: g.Number, Cause: c})
}

// failFrom maps a provider error: CONTENT_FILTERED → its own cause, a cancellation of ctx →
// nothing, anything else → CauseProvider. stage is where the error came from.
func (g *Generation) failFrom(ctx context.Context, err error, stage string) {
	var pe *provider.Error
	isPE := errors.As(err, &pe)
	if ctx.Err() != nil && (errors.Is(err, context.Canceled) || (isPE && pe.Kind == provider.KindCancelled)) {
		return
	}
	class := "UNKNOWN"
	if isPE {
		class = string(pe.Kind)
	}
	if isPE && pe.Kind == provider.KindContentFiltered {
		g.fail(CauseContentFiltered, stage, class)
		return
	}
	g.fail(CauseProvider, stage, class)
}

// deliver emits the clean text and the sentences, and hands the sentences to runTTS. A sentence
// the parser has cut is always handed over, even when the LLM was cut meanwhile (expiry: parsed
// sentences are still spoken, §7.7.2/§7.11); only the end of the generation or of runTTS stops it.
func (g *Generation) deliver(clean string, out []prompt.Sentence, sentences chan<- prompt.Sentence) bool {
	if clean != "" {
		g.recordText(clean)
		if !g.emit(EvTextDelta{Gen: g.Number, Clean: clean}) {
			return false
		}
	}
	for _, s := range out {
		g.recordSentence(s)
		if !g.emit(EvSentence{Gen: g.Number, Sentence: s}) {
			return false
		}
		select {
		case sentences <- s:
		case <-g.ttsStopped:
			return false
		case <-g.ctx.Done():
			return false
		}
	}
	return true
}

// runLLM streams the answer (§7.7.4): DELTA → parser.Feed; DONE{STOP|MAX_TOKENS} → Close;
// DONE{TRUNCATED} → no Close, what was parsed is spoken (llm_finished=false); CONTENT_FILTERED →
// failure (S-9); other ERROR → failure (no vendor switch, D-017); DONE{CANCELLED} → nothing.
func (g *Generation) runLLM(sentences chan<- prompt.Sentence) {
	defer close(sentences)
	req := g.p.LLM
	req.RequestID = fmt.Sprintf("%s:%d", g.p.SessionID, g.Number)
	stream, err := g.pg.StreamLLM(g.llmCtx, req)
	if err != nil {
		g.failFrom(g.llmCtx, err, failStageLLM)
		return
	}
	defer func() { _ = stream.Close() }()
	parser := prompt.NewSentenceParser(g.p.MaxSentences)
	for {
		if g.llmCtx.Err() != nil { // a cut LLM wins over events already buffered: no parser.Close
			return
		}
		select {
		case <-g.llmCtx.Done():
			return
		case ev, ok := <-stream.Events():
			if !ok {
				if g.llmCtx.Err() == nil {
					g.fail(CauseProvider, failStageLLM, "NO_TERMINAL_EVENT") // ended without a terminal event
				}
				return
			}
			switch ev.Type {
			case provider.LLMDelta:
				clean, out := parser.Feed(ev.Text)
				if !g.deliver(clean, out, sentences) {
					return
				}
				if parser.Limited { // R-17: a normal end, not a provider failure
					g.p.Metrics.sentenceLimit(g.Kind.metricLabel())
					g.llmFinished.Store(true)
					g.llmCancel()
					return
				}
			case provider.LLMDone:
				g.llmDone(ev.FinishReason, parser, sentences)
				return
			case provider.LLMError:
				var err error = ev.Err
				if ev.Err == nil {
					err = errors.New("conversation: LLM error without detail")
				}
				g.failFrom(g.llmCtx, err, failStageLLM)
				return
			}
		}
	}
}

func (g *Generation) llmDone(reason provider.FinishReason, parser *prompt.SentenceParser, sentences chan<- prompt.Sentence) {
	switch reason {
	case provider.FinishStop, provider.FinishMaxTokens:
		_, out := parser.Close()
		if parser.Limited { // the MaxSentences-th sentence came out of Close (R-17)
			g.p.Metrics.sentenceLimit(g.Kind.metricLabel())
		}
		if g.deliver("", out, sentences) {
			g.llmFinished.Store(true)
		}
	case provider.FinishTruncated: // S-7: the parser is not closed
		g.p.Metrics.llmTruncatedInc()
	case provider.FinishContentFiltered:
		g.fail(CauseContentFiltered, failStageLLM, string(provider.KindContentFiltered))
	case provider.FinishCancelled: // the loop's cancellation
	default:
		g.fail(CauseProvider, failStageLLM, "UNEXPECTED_FINISH")
	}
}

// runStatic feeds the static opening line through the parser without a sentence limit (S-2).
func (g *Generation) runStatic(openingLine string, sentences chan<- prompt.Sentence) {
	defer close(sentences)
	parser := prompt.NewSentenceParser(math.MaxInt)
	clean, out := parser.Feed(openingLine)
	if !g.deliver(clean, out, sentences) {
		return
	}
	_, out = parser.Close()
	if g.deliver("", out, sentences) {
		g.llmFinished.Store(true)
	}
}

// playAsset sends the pre-rendered apology clip as one sentence (§7.7.6).
func (g *Generation) playAsset(frames chan<- audio.OutFrame) {
	defer close(frames)
	pkts, err := g.p.Asset()
	if err != nil {
		g.fail(CauseProvider, failStageTTS, "ASSET")
		return
	}
	if len(pkts) == 0 {
		return // silent: the pacer reports it
	}
	g.llmFinished.Store(true) // no LLM: the content is complete
	g.recordSentence(prompt.Sentence{Index: 0, Text: g.p.AssetText})
	if !g.emit(EvSentence{Gen: g.Number, Sentence: prompt.Sentence{Index: 0, Text: g.p.AssetText}}) {
		return
	}
	for i, pkt := range pkts {
		f := audio.OutFrame{Gen: g.Number, Seq: uint32(i), Opus: pkt, LastOfSentence: i == len(pkts)-1}
		select {
		case frames <- f:
		case <-g.ctx.Done():
			return
		}
	}
}

// runPacer paces frames into Outbound. A generation that ends with zero frames sent is a
// silent generation: no AUDIO_WRITTEN_ALL will come, so it is reported as GENERATION_FAILED.
func (g *Generation) runPacer(frames <-chan audio.OutFrame) {
	hooks := PacerHooks{
		Admit: g.admitFrame,
		BeforeFirst: func() {
			if g.pg != nil {
				g.pg.MarkAudioSent() // S-12: right before the first frame is queued
			}
		},
		OnFirst: func() { g.emit(EvFirstAudio{Gen: g.Number}) },
		OnSentenceStart: func(f audio.OutFrame) {
			g.emit(EvSentenceAudio{Gen: g.Number, Index: f.SentenceIdx, FirstFrameSeq: f.Seq})
		},
		OnDone: func(n int, _ int64) {
			if n == 0 {
				g.fail(CauseProvider, failStageTTS, "NO_AUDIO")
			}
		},
	}
	_ = g.pacer.Run(g.ctx, frames, g.p.Out, hooks)
}

// admitFrame drops frames of a superseded TTS epoch and every frame of a failed generation. The
// decision and firstAdmitted are taken under epochMu, the lock restart takes to move the epoch:
// a frame is either dropped (the new epoch came first) or admitted before restart looks.
func (g *Generation) admitFrame(f audio.OutFrame) bool {
	g.epochMu.Lock()
	ok := f.Epoch >= g.ttsEpoch && !g.failed.Load()
	if ok {
		g.firstAdmitted = true
	}
	g.epochMu.Unlock()
	if !ok {
		g.p.Metrics.audioOut("dropped_stale", 1)
		return false
	}
	g.framesQueued.Add(1)
	return true
}

// runTTS synthesises the sentences one at a time, in order (one active Synthesize per
// generation), frames the PCM into 20 ms blocks and encodes them (one encoder per epoch).
func (g *Generation) runTTS(sentences <-chan prompt.Sentence, frames chan<- audio.OutFrame) {
	defer close(frames)
	defer close(g.ttsStopped)
	st := &ttsStage{g: g, frames: frames, in: sentences, epochCh: g.pg.TTSEpochChanged()}
	if !st.newEncoder() {
		return
	}
	for !g.failed.Load() {
		if st.next < len(st.parsed) {
			switch st.speak(st.parsed[st.next]) {
			case spoke:
				st.next++
			case restarted:
			case stopped:
				return
			}
			continue
		}
		if st.in == nil {
			return // every sentence spoken
		}
		select {
		case <-g.ctx.Done():
			return
		case e := <-st.epochCh:
			if !st.restart(e) {
				return
			}
		case s, ok := <-st.in:
			if !ok {
				st.in = nil
				continue
			}
			st.parsed = append(st.parsed, s)
		}
	}
}

type speakResult int

const (
	spoke     speakResult = iota // the sentence's frames are queued
	restarted                    // TTS moved to a new epoch: start again from sentence 0
	stopped                      // cancelled or failed
)

// ttsStage is the state of runTTS.
type ttsStage struct {
	g        *Generation
	frames   chan<- audio.OutFrame
	in       <-chan prompt.Sentence // nil once closed
	epochCh  <-chan int
	parsed   []prompt.Sentence // every sentence received, for an epoch restart
	next     int
	epoch    int
	enc      *audio.Encoder
	seq      uint32
	prevText string
}

func (st *ttsStage) newEncoder() bool {
	enc, err := audio.NewEncoder(provider.TTSSampleRate, st.g.p.OpusBitrate, st.g.p.OpusComplexity)
	if err != nil {
		st.g.fail(CauseProvider, failStageTTS, "ENCODER")
		return false
	}
	st.enc = enc
	return true
}

// restart moves to epoch e: frames of older epochs are dropped by admitFrame and every
// sentence is synthesised again from 0 with a fresh encoder and Seq 0.
func (st *ttsStage) restart(e int) bool {
	g := st.g
	if e <= st.epoch {
		return true // nothing new
	}
	st.epoch = e
	g.epochMu.Lock()
	g.ttsEpoch = e
	admitted := g.firstAdmitted
	g.epochMu.Unlock()
	if admitted {
		// the switch raced the first frame (MarkAudioSent): audio of the old voice is on its way
		// and the voice cannot change once audio is sent (S-12)
		g.fail(CauseProvider, failStageTTS, "EPOCH_RACE")
		return false
	}
	st.next, st.seq, st.prevText = 0, 0, ""
	return st.newEncoder()
}

func (st *ttsStage) restartResult(e int) speakResult {
	if st.restart(e) {
		return restarted
	}
	return stopped
}

func (st *ttsStage) send(f audio.OutFrame) bool {
	select {
	case st.frames <- f:
		return true
	case <-st.g.ctx.Done():
		return false
	}
}

func (st *ttsStage) encode(block []int16, idx int) (audio.OutFrame, bool) {
	pkt, err := st.enc.Encode(block)
	if err != nil {
		st.g.fail(CauseProvider, failStageTTS, "ENCODER")
		return audio.OutFrame{}, false
	}
	f := audio.OutFrame{Gen: st.g.Number, Seq: st.seq, SentenceIdx: idx, Opus: pkt, Epoch: st.epoch}
	st.seq++
	return f, true
}

// speak synthesises one sentence. The last encoded frame is held back until the next one or
// the Final chunk so it can carry LastOfSentence.
func (st *ttsStage) speak(s prompt.Sentence) speakResult {
	g := st.g
	req := provider.TTSRequest{
		Text: s.Text, Language: ttsLanguage, Voice: g.p.Voice, SpeakingRate: g.speakingRate, PreviousText: st.prevText,
		RequestID: fmt.Sprintf("%s:%d:%d", g.p.SessionID, g.Number, s.Index),
	}
	stream, err := g.pg.Synthesize(g.ctx, req)
	if err != nil {
		g.failFrom(g.ctx, err, failStageTTS)
		return stopped
	}
	defer func() { _ = stream.Close() }()
	framer := audio.NewFramer(provider.TTSSampleRate)
	var held *audio.OutFrame
	push := func(block []int16) bool {
		if held != nil && !st.send(*held) {
			return false
		}
		f, ok := st.encode(block, s.Index)
		held = &f
		return ok
	}
	for {
		select {
		case <-g.ctx.Done():
			return stopped
		case e := <-st.epochCh:
			return st.restartResult(e)
		case c, ok := <-stream.Chunks():
			if !ok {
				return st.endedWithoutFinal()
			}
			if c.Epoch < st.epoch {
				continue // a chunk of a superseded epoch
			}
			if c.Epoch > st.epoch { // the router switched before TTSEpochChanged was read here
				return st.restartResult(c.Epoch)
			}
			if c.Err != nil {
				if c.Err.VendorCode == vendorCodeTTSSwitched {
					return st.awaitEpoch()
				}
				g.failFrom(g.ctx, c.Err, failStageTTS)
				return stopped
			}
			for _, b := range framer.Write(c.PCM) {
				if !push(b) {
					return stopped
				}
			}
			if !c.Final {
				continue
			}
			if b := framer.Flush(); b != nil && !push(b) {
				return stopped
			}
			if held != nil {
				held.LastOfSentence = true
				if !st.send(*held) {
					return stopped
				}
			}
			st.prevText = s.Text
			return spoke
		}
	}
}

// awaitEpoch waits for the epoch announced by a tts_provider_switched error.
func (st *ttsStage) awaitEpoch() speakResult {
	select {
	case <-st.g.ctx.Done():
		return stopped
	case e := <-st.epochCh:
		return st.restartResult(e)
	}
}

// endedWithoutFinal handles a stream closed with neither Final nor Err: cancelled, superseded by
// an epoch switch, or broken.
func (st *ttsStage) endedWithoutFinal() speakResult {
	if st.g.ctx.Err() != nil {
		return stopped
	}
	select {
	case e := <-st.epochCh:
		return st.restartResult(e)
	default:
	}
	st.g.fail(CauseProvider, failStageTTS, "STREAM_ENDED")
	return stopped
}
