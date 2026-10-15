package conversation

import (
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/nabePi/kotoki-backend/internal/conversation/protocol"
	"github.com/nabePi/kotoki-backend/internal/conversation/rtstate"
	"github.com/nabePi/kotoki-backend/internal/memory"
	"github.com/nabePi/kotoki-backend/internal/provider"
	"github.com/nabePi/kotoki-backend/internal/session"
)

// Values of stt.final.end_reason (§7.4.5) and of the turn_end_reason_total label.
const (
	endReasonSilence      = "SILENCE"
	endReasonMaxUtterance = "MAX_UTTERANCE"
)

const (
	partialEvery       = 100 * time.Millisecond  // stt.partial throttle (≤ 10/s) and ai.text_delta coalescing
	memoryRefreshLimit = 1500 * time.Millisecond // RefreshEpisodic / ReloadCoreIfChanged budget (§7.7.5)
	minRefreshRunes    = 6                       // RefreshEpisodic is skipped for a shorter user_text
	maxRecentTurns     = 12                      // recentTurns ring (§7.7.5 step 1)
	recentForMemory    = 3                       // turns handed to RefreshEpisodic
	summaryMaxRunes    = 300
	defaultSTTFinal    = 1500 * time.Millisecond // PROVIDER_TIMEOUT_STT_FINAL
	maxAdaptiveTexts   = 6
)

// turnRun is the turn in progress (one session_turns row, §7.6.6). Only Runtime.loop touches it.
// An AI-initiated turn (the opening) only has the AI side; a user turn has both, and the user
// side is what the rows 1–9 of §7.6.1 move through.
type turnRun struct {
	id        uuid.UUID
	seq       int
	initiator string
	createdAt time.Time

	// User side.
	onsetMs     int64     // audio time of the onset (the detector's)
	startedAt   time.Time // wall clock of the onset: user_started_at
	userEndedAt time.Time // wall clock of the end of speech: user_ended_at; zero while the user speaks
	endReason   string    // SILENCE | MAX_UTTERANCE
	endAt       time.Time // when the end was detected: t0 of turn_end_to_first_audio (§7.14)
	noMerge     bool      // set by MAX_UTTERANCE: speech after it starts a new turn, never merges (row 4/9)
	voicedMs    int64     // voiced time of the utterance, for isDiscard

	waitingFinal bool      // the TURN_END commit is out and the turn waits for its FINAL (rows 3 → 7/8)
	commit       CommitID  // the TURN_END commit waited for
	finalizeAt   time.Time // when Finalize was called, for stt_final_ms

	hasUser    bool   // row 7 ran: userText is the text of the turn
	userText   string // the text the generation answers
	sttFinalMS int    // stt_final_ms of the payload; -1 after a STT_FINAL_TIMEOUT

	// failed is the final row of a turn that is FAILED and waits for its APOLOGY to end (rows 11,
	// 16, non-Japanese text); the turn is persisted and completed when the apology ends (row 15)
	// or is cut (barge-in, end of session). nil for every other turn.
	failed *session.TurnFinal
}

// recentTurn is one turn of the recentTurns ring (§7.7.5 step 1): what the prompt sees and what
// the user actually heard (for the memory refresh).
type recentTurn struct {
	ctx    TurnContext
	spoken string
}

// onDetector is the transition table of §7.6.1 for what the detector reports. Row numbers are those
// of the spec.
func (r *Runtime) onDetector(ev Event) {
	switch ev.Kind {
	case EventSpeechConfirmed:
		r.onSpeechConfirmed(ev.OnsetMs)
	case EventSpeechPause: // row 5
		if r.turnState == StateUserSpeaking && r.turn != nil {
			r.emit("speech.ended", evMeta{turn: r.turn}, protocol.SpeechEnded{
				AudioOffsetMS: r.det.lastVoicedMs, SpeechMS: int(max(0, r.det.lastVoicedMs-r.turn.onsetMs)),
			})
		}
	case EventSpeechResume: // row 6
		if r.turnState == StateUserSpeaking && r.turn != nil {
			r.emit("speech.started", evMeta{turn: r.turn}, protocol.SpeechStarted{AudioOffsetMS: r.det.NowMs() - bargeOnsetBackMs})
		}
	case EventSilenceReached: // row 3
		r.endUserTurn(endReasonSilence, ev.ThresholdMs)
	case EventMaxUtterance: // row 4
		r.endUserTurn(endReasonMaxUtterance, 0)
	case EventBargeInConfirmed: // row 13
		r.onBargeInConfirmed(ev.OnsetMs)
	}
}

// onSpeechConfirmed handles SPEECH_CONFIRMED: row 1 in LISTENING, row 2 when no turn is accepted,
// rows 9/9b in PROCESSING. The detector repeats the event on every voiced frame until the state
// changes, so every branch that does nothing must be cheap.
func (r *Runtime) onSpeechConfirmed(onsetMs int64) {
	if !r.accepting {
		return // row 2
	}
	switch r.turnState {
	case StateListening:
		r.beginUserTurn(onsetMs, false)
	case StateProcessing:
		r.onSpeechInProcessing(onsetMs)
	}
}

// beginUserTurn is row 1: turn_seq, INSERT (async), the STT mark, speech.started and turn.started.
// duringAI marks the turn a barge-in begins (speech.started{during_ai_speech}, §7.6.5).
func (r *Runtime) beginUserTurn(onsetMs int64, duringAI bool) {
	now := r.d.Clock.Now()
	t := &turnRun{
		id: r.d.IDs.New(), seq: r.nextTurnSeq, initiator: initiatorUser, createdAt: now,
		onsetMs: onsetMs, startedAt: now.Add(-time.Duration(r.det.NowMs()-onsetMs) * time.Millisecond),
		voicedMs: int64(r.det.win15.Count()) * FrameMs, // the start window is part of the utterance
	}
	r.nextTurnSeq++
	r.turn = t
	r.turnCount++
	r.submit(InsertTurn{Turn: TurnStart{ID: t.id, SessionID: r.sid, Sequence: t.seq, Generation: 0, CreatedAt: now}})
	if duringAI {
		r.sttSt.MarkTurnFrom(onsetMs)
	} else {
		r.sttSt.MarkTurn(onsetMs)
	}
	r.det.ResetForNewTurn()
	r.turnState = StateUserSpeaking
	r.partialDirty = false
	r.emit("speech.started", evMeta{turn: t}, protocol.SpeechStarted{AudioOffsetMS: onsetMs, DuringAISpeech: duringAI})
	r.emit("turn.started", evMeta{turn: t}, protocol.TurnStarted{Initiator: initiatorUser, StartedAt: protocol.Timestamp{Time: t.startedAt}})
	r.pushTurn()
	r.pushRT()
}

// endUserTurn is rows 3/4: the user turn ends, the TURN_END commit is made (S-8 (a)) and the turn
// waits for its FINAL under STT_FINAL_TIMEOUT.
func (r *Runtime) endUserTurn(reason string, thresholdMs int64) {
	t := r.turn
	if t == nil || r.turnState != StateUserSpeaking {
		return
	}
	now := r.d.Clock.Now()
	t.endReason, t.endAt, t.userEndedAt = reason, now, now
	if reason == endReasonSilence { // the speech ended when the last voiced cluster did
		t.userEndedAt = now.Add(-time.Duration(r.det.NowMs()-r.det.lastVoicedMs) * time.Millisecond)
	}
	t.noMerge = reason == endReasonMaxUtterance
	r.d.Metrics.TurnEndReason(r.endReasonLabel(reason, thresholdMs))
	r.turnState = StateProcessing
	t.waitingFinal = true
	var err error
	if r.stt != nil {
		err = r.stt.Finalize()
	} else {
		err = provider.ErrSessionClosed
	}
	if err != nil { // nothing to wait for: the text of the turn is what STT has given so far
		r.lg().Warn("stt finalize failed: using the running text", "error", err.Error())
		r.resolveUserText(true, false)
		return
	}
	t.commit = r.sttSt.Commit(CommitTurnEnd)
	t.finalizeAt = now
	r.dl.sttFinal = now.Add(r.d.STTFinalTimeout)
	r.pushTurn()
	r.pushRT()
}

// endReasonLabel is the turn_end_reason_total label of the silence threshold that ended the turn.
func (r *Runtime) endReasonLabel(reason string, thresholdMs int64) string {
	if reason == endReasonMaxUtterance {
		return "max_utterance"
	}
	c := r.det.cfg
	switch thresholdMs {
	case c.FillerOnlyMs:
		return "silence_filler_only"
	case c.CompleteMs:
		return "silence_500"
	case c.FillerMs:
		return "silence_1400"
	}
	return "silence_800"
}

// onSpeechInProcessing is rows 9 and 9b: speech while the turn waits for the STT or for its first
// audio. A user turn continues (same turn) unless it was ended by MAX_UTTERANCE, is as long as
// MAX_UTTERANCE or is answered by an APOLOGY; an opening that has not been heard yet is
// interrupted and the user's turn starts.
func (r *Runtime) onSpeechInProcessing(onsetMs int64) {
	t, g := r.turn, r.gen
	if t == nil {
		return
	}
	if t.initiator == initiatorAI { // row 9b
		if g != nil && g.kind == GenOpening && !g.audioStarted {
			r.interruptOpening(onsetMs)
		}
		return
	}
	if g != nil && (g.kind != GenReply || g.audioStarted) {
		return // an APOLOGY plays out; a reply that already speaks is a barge-in, not a resume
	}
	if t.noMerge || r.det.NowMs()-t.onsetMs >= int64(r.d.Cfg.TurnMaxUtteranceMS) {
		return // row 9 guard: no merge after MAX_UTTERANCE, utterance < 45 s
	}
	r.resumeUserTurn(onsetMs)
}

// resumeUserTurn is row 9: the generation (if one started) is cancelled with ai.interrupted
// {USER_RESUMED}, the STT timer stops, user_ended_at is cleared and the same turn goes on.
func (r *Runtime) resumeUserTurn(onsetMs int64) {
	t := r.turn
	if r.gen != nil {
		r.interruptGeneration("USER_RESUMED")
	}
	r.dl.sttFinal = time.Time{}
	t.waitingFinal = false
	r.sttSt.ResumeTurn()
	t.userEndedAt, t.endReason, t.endAt = time.Time{}, "", time.Time{}
	r.d.Metrics.TurnResumed()
	r.det.ResetForNewTurn()
	r.turnState = StateUserSpeaking
	r.partialDirty = false
	r.emit("speech.started", evMeta{turn: t}, protocol.SpeechStarted{AudioOffsetMS: onsetMs})
	r.pushTurn()
	r.pushRT()
}

// interruptOpening is row 9b: the opening was not heard yet, so turn 0 is INTERRUPTED with nothing
// spoken and the user's turn begins (row 1).
func (r *Runtime) interruptOpening(onsetMs int64) {
	t, g := r.turn, r.gen
	r.interruptGeneration("USER_RESUMED")
	fin := r.finalOf(g, session.TurnInterrupted, "")
	r.submit(FinalizeTurn{TurnID: t.id, Final: fin})
	r.d.Metrics.Turn(string(session.TurnInterrupted))
	text, spoken := g.g.Text(), ""
	r.emit("turn.completed", evMeta{turn: t}, protocol.TurnCompleted{
		Status: string(session.TurnInterrupted), AssistantText: &text, AssistantSpokenText: &spoken,
	})
	r.turn = nil
	r.sync.ClearTurn()
	r.postTurn(t, session.TurnInterrupted, "", "")
	r.beginUserTurn(onsetMs, false)
}

// ---- STT ----

// onSTTEvent is the entry of STT events into the turn engine: partials and finals feed sttState,
// the FINAL of the TURN_END commit decides the text of the turn (rows 7/8), a FINAL of a DISCARD
// commit is counted and dropped (S-8). An STT error ends the session (sttFailed).
func (r *Runtime) onSTTEvent(ev provider.STTEvent) {
	switch ev.Type {
	case provider.STTPartial:
		r.sttSt.OnPartial(ev)
		r.emitPartial(r.d.Clock.Now())
	case provider.STTFinal:
		res := r.sttSt.OnFinal(ev)
		switch res.Outcome {
		case FinalDiscarded:
			r.d.Metrics.STTDiscarded(r.discardAt[res.Commit])
			delete(r.discardAt, res.Commit)
		case FinalTurnEnd:
			if t := r.turn; t != nil && t.waitingFinal && t.commit == res.Commit {
				r.resolveUserText(false, true)
			}
		case FinalSegment, FinalTurnEndLate, FinalTurnEndSuperseded:
			// the text is kept (or dropped) by sttState; nothing to do for the engine
		}
	case provider.STTError:
		// §7.7.6, §7.11.4: the router already rotated the vendor's connection when it could, and
		// there is no other vendor: an STT error ends the session PROVIDER_FAILURE.
		r.sttFailed(ev)
	case provider.STTClosed:
		r.lg().Info("stt closed")
	}
}

// emitPartial sends stt.partial (the cumulative text of the turn) at most every 100 ms; a partial
// held back goes out on the next tick (flushPartial).
func (r *Runtime) emitPartial(now time.Time) {
	if !r.partialWanted() {
		return
	}
	if !r.lastPartialAt.IsZero() && now.Sub(r.lastPartialAt) < partialEvery {
		r.partialDirty = true
		return
	}
	r.sendPartial(now)
}

// partialWanted: stt.partial belongs to a user turn that is being spoken or waits for its FINAL.
func (r *Runtime) partialWanted() bool {
	t := r.turn
	if t == nil || t.initiator != initiatorUser {
		return false
	}
	return r.turnState == StateUserSpeaking || (r.turnState == StateProcessing && t.waitingFinal)
}

func (r *Runtime) sendPartial(now time.Time) {
	r.partialDirty = false
	text := r.sttSt.TurnText()
	if text == "" {
		return
	}
	r.lastPartialAt = now
	r.emit("stt.partial", evMeta{turn: r.turn}, protocol.STTPartial{Text: text})
}

// flushPartial sends the partial that the throttle held back, once 100 ms have passed.
func (r *Runtime) flushPartial(now time.Time) {
	if r.partialDirty && r.partialWanted() && now.Sub(r.lastPartialAt) >= partialEvery {
		r.sendPartial(now)
	}
}

// onSTTFinalTimeout is STT_FINAL_TIMEOUT (§7.6.2): the turn goes on with TurnText() as it is.
func (r *Runtime) onSTTFinalTimeout() {
	t := r.turn
	if t == nil || !t.waitingFinal {
		return
	}
	r.sttSt.ExpireTurnEnd(t.commit)
	r.resolveUserText(true, true)
}

// resolveUserText is rows 7/8: the text of the turn is known (the FINAL of the TURN_END commit, or
// the timeout), so it is either noise (DISCARDED) or answered. timedOut marks a STT_FINAL_TIMEOUT
// (stt_final_ms −1 / NULL); report says the router must hear about it (not when Finalize itself
// failed).
func (r *Runtime) resolveUserText(timedOut, report bool) {
	t := r.turn
	if t == nil || !t.waitingFinal {
		return
	}
	t.waitingFinal = false
	r.partialDirty = false
	r.dl.sttFinal = time.Time{}
	now := r.d.Clock.Now()
	t.sttFinalMS = -1
	var sttFinal *int
	if timedOut {
		if report && r.stt != nil {
			r.stt.ReportFinalTimeout()
		}
	} else {
		ms := int(now.Sub(t.finalizeAt) / time.Millisecond)
		t.sttFinalMS, sttFinal = ms, &ms
		r.d.Metrics.STTFinal(r.sttProvider(), now.Sub(t.finalizeAt))
	}
	text := strings.TrimSpace(r.sttSt.TurnText())
	if isDiscard(text, t.voicedMs) { // row 8
		r.discardTurn()
		return
	}
	// Row 7: UPDATE user_text (async), stt.final, then the reply.
	audioMs := int(r.sttAudioTotal() - r.sttAudioBase)
	started, ended := t.startedAt, t.userEndedAt
	user := TurnUser{CreatedAt: t.createdAt, Text: text, StartedAt: &started, EndedAt: &ended,
		STTProvider: nonEmpty(r.sttProvider()), STTFinalMS: sttFinal, STTAudioMS: &audioMs}
	r.submit(UpdateUser{TurnID: t.id, User: user})
	t.hasUser, t.userText = true, text
	r.emit("stt.final", evMeta{turn: t}, protocol.STTFinal{
		Text: text, EndReason: t.endReason, STTFinalMS: t.sttFinalMS, Provider: r.sttProvider(),
	})
	if isNonJapanese(text) { // the user did not speak Japanese: no reply, an APOLOGY, outside the failure streak
		r.failNonJapanese()
		return
	}
	if err := r.startGeneration(GenReply); err != nil {
		r.lg().Error("start reply generation failed", "error", err.Error())
		r.failReply(nil, CauseProvider)
		return
	}
	r.pushTurn()
	r.pushRT()
}

func (r *Runtime) sttProvider() string {
	if r.stt == nil {
		return ""
	}
	return string(r.stt.Provider())
}

// sttAudioTotal is the audio the STT stream has been sent (RoutedSTTSession.Stats, §7.6.6
// stt_audio_ms).
func (r *Runtime) sttAudioTotal() int64 {
	if r.stt == nil {
		return 0
	}
	return r.stt.Stats().AudioMsSent
}

// discardTurn is row 8: the text is noise, the turn is DISCARDED (persisted, completed) and the
// session listens again. There is no generation, no APOLOGY and the failure counter is untouched.
func (r *Runtime) discardTurn() {
	t := r.turn
	r.submit(FinalizeTurn{TurnID: t.id, Final: TurnFinal{CreatedAt: t.createdAt, Status: session.TurnDiscarded, CompletedAt: r.d.Clock.Now()}})
	r.d.Metrics.Turn(string(session.TurnDiscarded))
	r.turnCount-- // turn_count excludes DISCARDED turns (§7.4.5 session.completed)
	r.emit("turn.completed", evMeta{turn: t}, protocol.TurnCompleted{Status: string(session.TurnDiscarded)})
	r.turn = nil
	r.sync.ClearTurn()
	r.sttAudioBase = r.sttAudioTotal()
	r.commitBoundary("listening") // S-8 (c)
	r.turnState = StateListening
	r.pushRT()
}

// commitBoundary is the S-8 commit at the (b) entry into AI_SPEAKING and (c) return to LISTENING:
// stt.Finalize with purpose DISCARD, so a FINAL from audio before the boundary (an echo of the AI)
// never becomes the text of a turn. boundary is the label of stt_discarded_commits_total.
func (r *Runtime) commitBoundary(boundary string) {
	if r.stt == nil {
		return
	}
	if err := r.stt.Finalize(); err != nil {
		r.lg().Warn("stt finalize at boundary failed", "boundary", boundary, "error", err.Error())
		return
	}
	id := r.sttSt.Commit(CommitDiscard)
	r.discardAt[id] = boundary
}

// ---- after the turn ----

// postTurn is §7.7.5 for a turn that ended COMPLETED or INTERRUPTED: the failure streak resets, the
// turn joins recentTurns, the adaptive state adjusts (COMPLETED only), and the rolling summary and
// the memory refresh run in the background. assistant is the text the prompt shows for the turn
// (the spoken text for an INTERRUPTED one, D-078); spoken is what the user heard. A FAILED turn
// joins recentTurns too (the prompt shows the user's side of it) but changes nothing else: its
// streak was counted when it failed, and there is nothing to adapt to or to remember.
func (r *Runtime) postTurn(t *turnRun, status session.TurnStatus, assistant, spoken string) {
	if status != session.TurnFailed {
		r.failedTurns = 0
	} else if !t.hasUser {
		return // a FAILED opening has nothing the prompt could show
	}
	tc := TurnContext{Seq: t.seq, Status: string(status), AssistantText: assistant}
	if t.hasUser {
		u := t.userText
		tc.UserText = &u
	}
	r.recent = append(r.recent, recentTurn{ctx: tc, spoken: spoken})
	if len(r.recent) > maxRecentTurns {
		r.recent = append([]recentTurn(nil), r.recent[len(r.recent)-maxRecentTurns:]...)
	}
	if status == session.TurnCompleted {
		r.adjustAdaptive(t)
	}
	r.maybeSummarize()
	if status != session.TurnFailed {
		r.refreshMemory(t)
	}
	r.pushRT()
}

// recentContexts is the recentTurns ring as the prompt builder takes it.
func (r *Runtime) recentContexts() []TurnContext {
	out := make([]TurnContext, len(r.recent))
	for i, rt := range r.recent {
		out[i] = rt.ctx
	}
	return out
}

// adjustAdaptive is §7.7.5 step 1b / §7.8.6: pure, on the loop, effective from the next
// generation.
func (r *Runtime) adjustAdaptive(t *turnRun) {
	var texts []string
	for _, rt := range r.recent {
		if rt.ctx.UserText != nil {
			texts = append(texts, *rt.ctx.UserText)
		}
	}
	if len(texts) > maxAdaptiveTexts {
		texts = texts[len(texts)-maxAdaptiveTexts:]
	}
	next, changed := MaybeAdjust(r.adaptive, texts)
	r.adaptive = next
	if !changed {
		return
	}
	if r.d.Learner != nil {
		r.learnerCtx = r.d.Learner.Rerender(r.learnerCtx, next.Current)
	}
	r.d.Metrics.difficultyAdjustedInc()
	c := next.Current
	r.submit(SessionEvent{Type: string(session.EventDifficulty), Payload: map[string]int{
		"turn_seq": t.seq, "in_session_adjustments": c.InSessionAdjustments, "speech_speed": c.SpeechSpeed,
		"question_complexity": c.QuestionComplexity, "scaffolding": c.Scaffolding,
	}})
}

// maybeSummarize is §7.8.5: a summary job starts when enough turns fell out of the window.
func (r *Runtime) maybeSummarize() {
	if r.d.Router == nil {
		return
	}
	if r.summarizer == nil {
		r.summarizer = NewSummarizer(r.d.Router, r.d.Builder, r.d.Cfg.RollingSummaryTimeout, r.sid,
			WithSummaryModel(r.d.Cfg.RollingSummaryLLMModel), WithSummaryTrigger(r.d.Cfg.RollingSummaryTriggerTurns))
	}
	turns := r.recentContexts()
	// The trigger counts the turns behind the base window of 8, not behind the window
	// SelectRecentWindow widens to hold the turns no summary covers yet (§7.8.5).
	var live []TurnContext
	for _, t := range turns {
		if t.Status != statusDiscarded {
			live = append(live, t)
		}
	}
	if len(live) <= recentWindow {
		return
	}
	oldest := live[len(live)-recentWindow].Seq
	if !r.summarizer.ShouldRun(turns, r.coveredUntil, oldest, r.summaryRunning) {
		return
	}
	var cover []TurnContext
	for _, t := range live {
		if t.Seq > r.coveredUntil && t.Seq < oldest {
			cover = append(cover, t)
		}
	}
	in := SummaryInput{PreviousSummary: r.summary, Turns: cover, MaxRunes: summaryMaxRunes}
	r.summaryRunning = true
	r.goSafe("rolling summary", func() {
		res, err := r.summarizer.Run(r.ctx, in)
		r.post(r.ctx, func(r *Runtime) { r.onSummaryReady(res, err) })
	})
}

// onSummaryReady is SummaryReady: a newer summary replaces the cached one and goes to Redis.
func (r *Runtime) onSummaryReady(res SummaryResult, err error) {
	r.summaryRunning = false
	if err != nil {
		r.d.Metrics.RollingSummary("error")
		r.lg().Warn("rolling summary failed", "error", err.Error())
		return
	}
	r.d.Metrics.RollingSummary("ok")
	if res.CoveredUntilSeq <= r.coveredUntil {
		return
	}
	r.summary, r.coveredUntil = res.Text, res.CoveredUntilSeq
	r.sync.UpdateSummary(rtstate.Summary{
		Text: res.Text, CoveredUntilSeq: int64(res.CoveredUntilSeq), Model: r.d.Cfg.RollingSummaryLLMModel,
		UpdatedAtMS: r.d.Clock.Now().UnixMilli(),
	})
}

// refreshMemory is §7.7.5 steps 3 and 4: in one goroutine, at most one in flight, the episodic
// memory is refreshed (skipped for a short user_text) and the core memory is reloaded when its
// version changed (S-15). The results replace the runtime's copies from the next generation on; a
// failure keeps what the runtime has.
func (r *Runtime) refreshMemory(t *turnRun) {
	if r.d.Memory == nil || r.memBusy {
		return
	}
	var recent []memory.TurnText
	for _, rt := range r.recent {
		if (rt.ctx.Status == string(session.TurnCompleted) || rt.ctx.Status == string(session.TurnInterrupted)) && rt.ctx.UserText != nil {
			recent = append(recent, memory.TurnText{UserText: *rt.ctx.UserText, AssistantSpokenText: rt.spoken})
		}
	}
	if len(recent) > recentForMemory {
		recent = recent[len(recent)-recentForMemory:]
	}
	refresh := t.hasUser && RuneLen(t.userText) >= minRefreshRunes && len(recent) > 0
	mc := r.mem // a copy: the goroutine reads it, the loop replaces slices and never edits them
	uid, sid, svc := r.uid, r.sid, r.d.Memory
	r.memBusy = true
	r.goSafe("memory refresh", func() {
		var episodic []memory.ContextItem
		refreshed := false
		if refresh {
			ctx, cancel := r.ctxWithDeadline(r.ctx, memoryRefreshLimit)
			items, err := svc.RefreshEpisodic(ctx, uid, sid, recent)
			cancel(nil)
			if err != nil {
				r.log.Warn("episodic memory refresh failed", "error", err.Error())
			} else {
				episodic, refreshed = items, true
			}
		}
		ctx, cancel := r.ctxWithDeadline(r.ctx, memoryRefreshLimit)
		changed, err := svc.ReloadCoreIfChanged(ctx, &mc)
		cancel(nil)
		if err != nil {
			r.log.Warn("memory core reload failed", "error", err.Error())
			changed = false
		}
		r.post(r.ctx, func(r *Runtime) {
			r.memBusy = false
			if refreshed {
				r.mem.Episodic, r.mem.EpisodicTokens = episodic, sumTokens(episodic)
			}
			if changed {
				r.mem.Core, r.mem.CoreTokens, r.mem.UserMemVer = mc.Core, mc.CoreTokens, mc.UserMemVer
			}
		})
	})
}

func sumTokens(items []memory.ContextItem) int {
	n := 0
	for _, it := range items {
		n += it.Tokens
	}
	return n
}
