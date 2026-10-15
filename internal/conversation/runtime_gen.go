package conversation

import (
	"fmt"
	"runtime/debug"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/nabePi/kotoki-backend/internal/conversation/protocol"
	"github.com/nabePi/kotoki-backend/internal/conversation/rtstate"
	"github.com/nabePi/kotoki-backend/internal/platform/errs"
	"github.com/nabePi/kotoki-backend/internal/prompt"
	"github.com/nabePi/kotoki-backend/internal/provider"
	"github.com/nabePi/kotoki-backend/internal/session"
)

// Initiators of turn.started.
const (
	initiatorAI   = "AI"
	initiatorUser = "USER"
)

// genRun is the generation of the turn in progress and what the loop keeps while it runs: the
// transcript the events build (assistant_text, per-sentence text for assistant_spoken_text) and
// the timing of the latency columns. The Generation itself is the pipeline of Task 12.
type genRun struct {
	g      *Generation
	kind   GenKind
	number uint32
	turn   *turnRun
	// static is the S-2 opening: opening_line spoken without an LLM (llm_provider stays NULL).
	static    bool
	startedAt time.Time
	// t0 is the start of turn_end_to_first_audio: the turn end detected by the detector (REPLY),
	// the start of the generation otherwise (§7.14).
	t0 time.Time

	text         strings.Builder // assistant_text: every clean delta
	pending      strings.Builder // text not yet sent as ai.text_delta (coalesced to 100 ms)
	pendingSince time.Time       // when the first pending delta arrived
	lastFlush    time.Time       // when ai.text_delta last went out
	firstSent    bool            // llm_first_sentence_seconds was observed
	llmSentence  time.Duration   // request → first sentence (llm_first_sentence_seconds)
	sentences    map[int]string  // parsed sentence text by index
	audioStarted bool
	audioAt      time.Time
	audioDone    bool
	audio        EvAudioDone // valid once audioDone
	cancelID     uint64      // the expires_at + PROVIDER_SESSION_GRACE cut of this generation
}

// spokenText is assistant_spoken_text: the first k sentences, in order (D-078). A turn that played
// to the end passes the sentence count. The texts come from the Generation, which records every
// sentence before it can be spoken: the loop's own copy (sentences) may still be behind the writer.
func (g *genRun) spokenText(k int) string {
	var b strings.Builder
	for i := range k {
		b.WriteString(g.g.SentenceText(i))
	}
	return b.String()
}

// startOpening is §7.6.3 step 9 for AI_FIRST: turn 0 (initiator AI, no user text), state
// PROCESSING and the OPENING generation. USER_FIRST never gets here.
func (r *Runtime) startOpening() {
	if r.cs == nil { // the client left during the start: the opening plays when one resumes
		r.log.Warn("opening postponed: no connection")
		r.openingPending = true
		return
	}
	now := r.d.Clock.Now()
	t := &turnRun{id: r.d.IDs.New(), seq: 0, initiator: initiatorAI, createdAt: now}
	r.turn = t
	r.turnCount++
	r.submit(InsertTurn{Turn: TurnStart{ID: t.id, SessionID: r.sid, Sequence: t.seq, Generation: 0, CreatedAt: now}})
	r.emit("turn.started", evMeta{turn: t}, protocol.TurnStarted{Initiator: initiatorAI, StartedAt: protocol.Timestamp{Time: now}})
	r.turnState = StateProcessing
	if err := r.startGeneration(GenOpening); err != nil {
		r.lg().Error("start opening generation failed", "error", err.Error())
		r.failTurn(errs.CodeAiGenerationFailed, CauseProvider, "")
		return
	}
	r.pushTurn()
	r.pushRT()
}

// startGeneration begins one AI generation of the current turn (§7.7.1 step 6): the prompt,
// generation++, the pipeline, ai.generation_started. OPENING without an OPENING prompt version is
// the static opening of S-2. It returns an error when the prompt cannot be built; nothing has
// started then, and the caller fails the turn.
func (r *Runtime) startGeneration(kind GenKind) error {
	t := r.turn
	static := kind == GenOpening && r.snap.Opening == nil
	var req provider.LLMRequest
	if kind != GenApology && !static {
		mode := ModeConversation
		userText := t.userText
		if kind == GenOpening {
			mode, userText = ModeOpening, ""
		}
		bp, err := r.buildPrompt(mode, userText)
		if err != nil {
			return fmt.Errorf("conversation: build prompt: %w", err)
		}
		req = llmRequest(bp)
	}
	r.generation++
	n := r.generation
	r.cs.activeGen.Store(n)
	p := genParams{
		Number: n, Kind: kind, TurnID: t.id, SessionID: r.sid, Parent: r.ctx,
		Router: r.d.Router, Voice: r.voice,
		VoiceSpeakingRate: r.snap.Snapshot.Character.Voice.SpeakingRate, AdaptiveRate: r.adaptive.Current.TTSSpeakingRate,
		MaxSentences: max(1, r.adaptive.Current.MaxAISentencesPerTurn), LLM: req,
		Out: r.cs.out, Events: r.genEvents,
		Clock: r.d.Clock, Sleep: r.d.Sleep, MaxLeadMS: int64(r.d.Cfg.AudioMaxLeadMS),
		OpusBitrate: r.d.Cfg.OpusBitrate, OpusComplexity: r.d.Cfg.OpusComplexity,
		Metrics: r.d.Metrics, Asset: r.d.Apology,
		OnPanic: func(v any) { signal(r.fatalCh, &panicError{who: "generation", value: v, stack: debug.Stack()}) },
	}
	g := newGeneration(p)
	run := &genRun{g: g, kind: kind, number: n, turn: t, static: static, startedAt: r.d.Clock.Now(), sentences: map[int]string{}}
	run.t0 = run.startedAt
	if kind == GenReply && !t.endAt.IsZero() {
		run.t0 = t.endAt
	}
	// §7.7.4: the generation context ends at expires_at + PROVIDER_SESSION_GRACE, on the runtime clock.
	run.cancelID = r.cancelAt(r.expiresAt.Add(r.d.ProviderSessionGrace), g.Cancel)
	r.gen = run
	r.emit("ai.generation_started", genMeta(t, n), protocol.AIGenerationStarted{Kind: string(kind)})
	switch {
	case kind == GenApology:
		g.startAsset()
	case static:
		r.lg().Warn("opening prompt version missing: playing the static opening line", "scenario_version_id", r.snap.ScenarioVersion.ID.String())
		r.d.Metrics.OpeningFallback()
		g.startStatic(r.snap.ScenarioVersion.OpeningLine)
	default:
		g.startLLM()
	}
	return nil
}

// buildPrompt assembles ConversationInput from the runtime caches (no I/O, §7.8.2): the recent
// turns window, the rolling summary and the user text the reply answers (empty for the opening).
func (r *Runtime) buildPrompt(mode Mode, userText string) (BuiltPrompt, error) {
	pv := r.snap.PromptVersion
	if mode == ModeOpening && r.snap.Opening != nil {
		pv = *r.snap.Opening
	}
	bp, err := r.d.Builder.BuildConversation(ConversationInput{
		Mode: mode, Prompt: pv, Config: r.snap.SessionConfig, Learner: r.learnerCtx, Adaptive: r.adaptive.Current,
		Memory: r.mem, RollingSummary: r.summary, RecentTurns: SelectRecentWindow(r.recentContexts(), r.coveredUntil),
		CurrentUserText: userText, RemainingSeconds: r.remainingSeconds(), Budget: prompt.DefaultBudget,
		DurationMinutes: (r.rec.DurationSeconds + 59) / 60, SessionMode: string(r.rec.ConversationStartMode),
	})
	if err != nil {
		return BuiltPrompt{}, err
	}
	for comp, tokens := range bp.ComponentTokens {
		r.d.Metrics.PromptTokens(comp, tokens)
	}
	return bp, nil
}

func llmRequest(bp BuiltPrompt) provider.LLMRequest {
	msgs := make([]provider.Message, len(bp.Messages))
	for i, m := range bp.Messages {
		msgs[i] = provider.Message{Role: provider.Role(m.Role), Content: m.Content}
	}
	return provider.LLMRequest{Messages: msgs, MaxTokens: bp.MaxOutputTokens, Temperature: float32ToDecimal(bp.Temperature)}
}

// cancelGeneration stops the active generation: generation++ (a cancelled number is never
// reused, D-077), the writer is told, frames of the old generation still queued are purged. The
// caller emits ai.interrupted when the client must hear about it.
func (r *Runtime) cancelGeneration() {
	if r.gen == nil {
		return
	}
	g := r.gen
	g.g.Cancel()
	r.retired = append(r.retired, g.g)
	// The loop never waits for the goroutines of a cancelled generation (§7.6.5); this one only
	// watches how long they take to leave (generation_goroutine_exit_seconds, must stay < 1 s).
	started, gg := r.d.Clock.Now(), g.g
	r.goSafe("generation exit", func() {
		gg.Wait()
		r.d.Metrics.GenerationExit(r.d.Clock.Now().Sub(started))
	})
	r.dropCancel(g.cancelID)
	r.generation++
	if r.cs != nil {
		r.cs.activeGen.Store(r.generation)
		r.cs.out.PurgeAudio(r.generation)
	}
	r.gen = nil
	r.dl.playback = time.Time{}
}

// interruptGeneration cancels the active generation and tells the client (ai.interrupted, with the
// new active generation and how many sentences were fully written). It returns that sentence
// count (assistant_spoken_text covers exactly those). Barge-in (Task 20) and the end of the
// session use it.
func (r *Runtime) interruptGeneration(reason string) int {
	g := r.gen
	if g == nil {
		return 0
	}
	spoken := 0
	if r.cs != nil {
		spoken = r.cs.writer.SentencesWritten(g.number)
	}
	return r.interruptGenerationWith(reason, spoken)
}

// interruptGenerationWith is interruptGeneration with the spoken-sentence count already known: a
// disconnect reads it from the writer of the connection that is going away.
func (r *Runtime) interruptGenerationWith(reason string, spoken int) int {
	g := r.gen
	if g == nil {
		return 0
	}
	r.flushText(g) // the text the generation produced reaches the client before it is interrupted
	r.cancelGeneration()
	r.emit("ai.interrupted", genMeta(g.turn, g.number), protocol.AIInterrupted{
		Reason: reason, ActiveGeneration: r.generation, SpokenSentenceCount: spoken,
	})
	r.d.Metrics.GenerationCancelled(reason)
	return spoken
}

// onGenEvent handles the events of the generation goroutines; an event of a generation that is
// no longer the active one is dropped (§7.7.2).
func (r *Runtime) onGenEvent(ev GenEvent) {
	r.handled++
	g := r.gen
	if g == nil || ev.GenNumber() != g.number {
		return
	}
	switch e := ev.(type) {
	case EvTextDelta:
		g.text.WriteString(e.Clean)
		g.pending.WriteString(e.Clean)
		now := r.d.Clock.Now()
		if g.pendingSince.IsZero() {
			g.pendingSince = now
		}
		if g.lastFlush.IsZero() || now.Sub(g.lastFlush) >= partialEvery {
			r.flushText(g)
		}
		r.pushTurn()
	case EvSentence:
		g.sentences[e.Sentence.Index] = e.Sentence.Text
		if !g.firstSent {
			g.firstSent = true
			if sum := g.g.Summary(); !g.static && sum.LLMProvider != "" {
				g.llmSentence = r.d.Clock.Now().Sub(g.startedAt)
				r.d.Metrics.LLMFirstSentence(string(sum.LLMProvider), sum.LLMModel, g.llmSentence)
			}
		}
	case EvFirstAudio:
		r.flushText(g)
		r.onFirstAudio(g)
	case EvSentenceAudio:
		r.flushText(g)
		r.emit("ai.sentence", genMeta(g.turn, g.number), protocol.AISentence{
			SentenceIndex: e.Index, Text: g.sentences[e.Index], FirstFrameSeq: e.FirstFrameSeq, StartMS: int(e.FirstFrameSeq) * FrameMs,
		})
	case EvAudioDone:
		r.flushText(g)
		r.onAudioDone(g, e)
	case EvGenFailed:
		r.flushText(g)
		r.onGenFailed(g, e)
	}
}

// flushText sends the coalesced ai.text_delta of the generation (spec §7.4.5: coalesce 100 ms).
// Every other event of the generation goes out after it, so the order of the stream is kept.
func (r *Runtime) flushText(g *genRun) {
	if g == nil || g.pending.Len() == 0 {
		return
	}
	text := g.pending.String()
	g.pending.Reset()
	g.pendingSince = time.Time{}
	g.lastFlush = r.d.Clock.Now()
	r.emit("ai.text_delta", genMeta(g.turn, g.number), protocol.AITextDelta{Text: text})
}

// flushDueText sends the text a generation has been holding back for 100 ms (on the loop tick).
func (r *Runtime) flushDueText(now time.Time) {
	if g := r.gen; g != nil && g.pending.Len() > 0 && now.Sub(g.pendingSince) >= partialEvery {
		r.flushText(g)
	}
}

// onFirstAudio is row 10: the first frame of the generation was queued after MarkAudioSent (S-12).
func (r *Runtime) onFirstAudio(g *genRun) {
	now := r.d.Clock.Now()
	g.audioStarted, g.audioAt = true, now
	r.turnState = StateAISpeaking
	r.emit("ai.audio_started", genMeta(g.turn, g.number), protocol.AIAudioStarted{
		AudioParams: protocol.AudioParams{Codec: "opus", SampleRate: 48000, Channels: 1, FrameMS: FrameMs},
	})
	sum := g.g.Summary()
	kind := strings.ToLower(string(g.kind))
	r.d.Metrics.TurnEndToFirstAudio(kind, sum.UsedFallback, now.Sub(g.t0))
	if !g.static && sum.LLMFirstTokenMs > 0 {
		r.d.Metrics.LLMFirstToken(string(sum.LLMProvider), sum.LLMModel, time.Duration(sum.LLMFirstTokenMs)*time.Millisecond)
	}
	if sum.UsedFallback && r.voice.Fallback != nil { // §7.7.6: TTS moved to voice.fallback, the one vendor fallback (D-017)
		id := g.turn.id
		r.submit(SessionEvent{Type: string(session.EventFallback), TurnID: &id, Payload: map[string]any{
			"capability": "TTS", "from": string(r.voice.Primary.Provider), "to": string(r.voice.Fallback.Provider), "turn_seq": g.turn.seq,
		}})
	}
	if sum.TTSFirstAudioMs > 0 {
		r.d.Metrics.TTSFirstAudio(string(sum.TTSProvider), sum.TTSModel, time.Duration(sum.TTSFirstAudioMs)*time.Millisecond)
	}
	if t := g.turn; g.kind == GenReply && !g.static && t.sttFinalMS >= 0 && g.llmSentence > 0 { // server_overhead_seconds (§7.14)
		providers := time.Duration(t.sttFinalMS)*time.Millisecond + g.llmSentence + time.Duration(sum.TTSFirstAudioMs)*time.Millisecond
		r.d.Metrics.ServerOverhead(max(0, now.Sub(g.t0)-providers))
	}
	r.commitBoundary("ai_speaking") // S-8 (b)
	r.pushTurn()
	r.pushRT()
}

// onWriterAck turns the writer's AUDIO_WRITTEN_ALL into AUDIO_DONE of the active generation.
func (r *Runtime) onWriterAck(a WriterAck) {
	r.handled++
	if r.gen == nil {
		return
	}
	if ev, ok := r.gen.g.OnWriterAck(a); ok {
		r.onAudioDone(r.gen, ev)
	}
}

// onAudioDone is row 14: every frame of the generation was written. The turn closes on
// client.playback_finished or at the end of the estimated playback plus PLAYBACK_FINISH_GRACE_MS.
func (r *Runtime) onAudioDone(g *genRun, e EvAudioDone) {
	g.audioDone, g.audio = true, e
	r.emit("ai.audio_completed", genMeta(g.turn, g.number), protocol.AIAudioCompleted{
		FrameCount: e.FrameCount, TotalAudioMS: int(e.TotalMS), SentenceCount: e.SentenceCount, LLMFinished: e.LLMFinished,
	})
	end := g.g.PlaybackEnd()
	if end.IsZero() {
		end = r.d.Clock.Now()
	}
	r.dl.playback = end.Add(time.Duration(r.d.Cfg.PlaybackFinishGraceMS) * time.Millisecond)
}

// onPlaybackFinished is row 15 (row 19 when it does not apply): the client reports the end of
// playback of the active generation.
func (r *Runtime) onPlaybackFinished(p protocol.ClientPlaybackFinished) {
	g := r.gen
	if r.turnState != StateAISpeaking || g == nil || !g.audioDone || p.Generation != g.number {
		return
	}
	r.closeTurn(r.playedOutStatus(g))
}

// playedOutStatus is the status of a turn whose audio played to the end: COMPLETED, except that a
// turn the end of the session cut short (the LLM was stopped at expires_at, §7.11.2) is
// INTERRUPTED. A TRUNCATED answer of a session that goes on is COMPLETED (S-7).
func (r *Runtime) playedOutStatus(g *genRun) session.TurnStatus {
	if r.end != nil && !g.audio.LLMFinished {
		return session.TurnInterrupted
	}
	return session.TurnCompleted
}

// onPlaybackDeadline is row 15 by timeout: no playback_finished came (playback_finished_missing).
func (r *Runtime) onPlaybackDeadline() {
	if r.turnState != StateAISpeaking || r.gen == nil || !r.gen.audioDone {
		return
	}
	r.d.Metrics.PlaybackFinishedMissing()
	r.closeTurn(r.playedOutStatus(r.gen))
}

// closeTurn finishes the turn after its audio played (row 15): persist, turn.completed, S-8 (c),
// the post-turn tasks of §7.7.5 and back to LISTENING.
func (r *Runtime) closeTurn(status session.TurnStatus) {
	t, g := r.turn, r.gen
	if t == nil || g == nil {
		return
	}
	if t.failed != nil { // the APOLOGY of a FAILED turn ended: the turn stays FAILED
		r.completeFailedTurn(r.end == nil)
		return
	}
	// assistant_spoken_text: the sentences whose last frame the writer has written. A fragment the
	// parser never cut (DONE{TRUNCATED}, S-7) is in assistant_text but was never spoken.
	spoken := g.spokenText(r.spokenCount(g))
	r.flushText(g)
	fin := r.finalOf(g, status, spoken)
	r.finalize(t.id, fin)
	r.d.Metrics.Turn(string(status))
	text, spokenText := g.g.Text(), spoken
	tc := protocol.TurnCompleted{Status: string(status), AssistantText: &text, AssistantSpokenText: &spokenText}
	if t.hasUser {
		u := t.userText
		tc.UserText = &u
		tc.Latency = r.latencyOf(t, fin)
	}
	r.emit("turn.completed", evMeta{turn: t}, tc)
	r.dropCancel(g.cancelID)
	r.gen, r.turn = nil, nil
	r.dl.playback = time.Time{}
	r.sync.ClearTurn()
	r.sttAudioBase = r.sttAudioTotal()
	r.turnState = StateListening
	if r.end == nil { // an ending session has nothing left to listen for or to learn from
		r.commitBoundary("listening") // S-8 (c)
		r.postTurn(t, status, text, spoken)
	}
	r.pushRT()
}

// spokenCount is how many sentences of g the user heard: the ones whose last frame the writer has
// written (D-078). Without a connection there is no writer to ask and the parsed sentences stand
// in. A fragment the parser never cut (DONE{TRUNCATED}) is in neither.
func (r *Runtime) spokenCount(g *genRun) int {
	if r.cs == nil {
		return len(g.sentences)
	}
	return r.cs.writer.SentencesWritten(g.number)
}

// latencyOf is the optional latency object of turn.completed.
func (r *Runtime) latencyOf(t *turnRun, fin session.TurnFinal) *protocol.Latency {
	l := &protocol.Latency{STTFinalMS: t.sttFinalMS}
	if fin.TurnEndToFirstAudioMS != nil {
		l.TurnEndToFirstAudioMS = *fin.TurnEndToFirstAudioMS
	}
	if fin.LLMFirstTokenMS != nil {
		l.LLMFirstTokenMS = *fin.LLMFirstTokenMS
	}
	if fin.TTSFirstAudioMS != nil {
		l.TTSFirstAudioMS = *fin.TTSFirstAudioMS
	}
	return l
}

// finalOf builds the columns of a final turn from the generation (§7.6.6). A static opening has
// no LLM: llm_provider, llm_model and the token counts stay NULL.
func (r *Runtime) finalOf(g *genRun, status session.TurnStatus, spoken string) session.TurnFinal {
	sum := g.g.Summary()
	text := g.g.Text()
	f := session.TurnFinal{
		CreatedAt: g.turn.createdAt, Status: status, AssistantText: &text, AssistantSpokenText: &spoken,
		TTSProvider: nonEmpty(string(sum.TTSProvider)), TTSModel: nonEmpty(sum.TTSModel), TTSVoiceID: nonEmpty(sum.TTSVoiceID),
		UsedFallback: sum.UsedFallback, CompletedAt: r.d.Clock.Now(),
	}
	if g.audioStarted {
		f.TurnEndToFirstAudioMS = ptrInt(int(g.audioAt.Sub(g.t0) / time.Millisecond))
	}
	if sum.TTSCharacters > 0 {
		f.TTSCharacters = ptrInt(sum.TTSCharacters)
	}
	if sum.TTSFirstAudioMs > 0 {
		f.TTSFirstAudioMS = ptrInt(int(sum.TTSFirstAudioMs))
	}
	if !g.static && g.kind != GenApology {
		f.LLMProvider, f.LLMModel = nonEmpty(string(sum.LLMProvider)), nonEmpty(sum.LLMModel)
		f.InputTokens, f.OutputTokens = ptrInt(sum.LLMUsage.InputTokens), ptrInt(sum.LLMUsage.OutputTokens)
		if sum.LLMFirstTokenMs > 0 {
			f.LLMFirstTokenMS = ptrInt(int(sum.LLMFirstTokenMs))
		}
	}
	return f
}

func nonEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func ptrInt(n int) *int { return &n }

// pushTurn hands the turn in progress to the redisSync (coalesced 250 ms).
func (r *Runtime) pushTurn() {
	t := r.turn
	if t == nil {
		return
	}
	snap := rtstate.TurnSnap{TurnID: t.id, TurnSeq: t.seq, Initiator: t.initiator, TurnState: string(r.turnState), Generation: int(r.generation)}
	if g := r.gen; g != nil {
		snap.AssistantText = g.text.String()
		if r.cs != nil {
			snap.SpokenSentences = r.cs.writer.SentencesWritten(g.number)
		}
	}
	r.sync.UpdateTurn(snap)
}

// finalize closes a turn row. The highest generation issued so far goes with it (§7.9.4: a takeover
// that cannot read rt continues above the largest session_turns.generation).
func (r *Runtime) finalize(id uuid.UUID, fin TurnFinal) {
	fin.Generation = ptrInt(int(r.generation))
	r.submit(FinalizeTurn{TurnID: id, Final: fin})
}

// submit queues a persister job. A refusal after a fence loss or a rejection is already being
// handled through the persister callbacks; any other refusal is logged.
func (r *Runtime) submit(job PersistJob) {
	if err := r.persister.Submit(job); err != nil {
		r.lg().Warn("persist job refused", "job", job.persistKind(), "error", err.Error())
	}
}
