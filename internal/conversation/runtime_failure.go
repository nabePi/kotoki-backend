package conversation

import (
	"fmt"
	"time"

	"github.com/nabePi/kotoki-backend/internal/conversation/protocol"
	"github.com/nabePi/kotoki-backend/internal/platform/errs"
	"github.com/nabePi/kotoki-backend/internal/prompt"
	"github.com/nabePi/kotoki-backend/internal/provider"
	"github.com/nabePi/kotoki-backend/internal/session"
)

// turn_error codes of turns that failed for a reason other than the generation.
const codeSTTNonJapanese = "STT_NON_JAPANESE"

// onGenFailed is GENERATION_FAILED (§7.6.1 rows 11, 12, 16): an OPENING that failed ends turn 0
// (row 12), a REPLY that failed before or after its first audio is answered with the APOLOGY
// (rows 11 and 16), an APOLOGY that failed ends its turn without a second one.
func (r *Runtime) onGenFailed(g *genRun, e EvGenFailed) {
	select { // a panic of the same generation is the real outcome: it reaches the loop on fatalCh
	case err := <-r.fatalCh:
		r.onFatalSignal(err)
		return
	default:
	}
	r.lg().Warn("generation failed", "kind", string(g.kind), "cause", string(e.Cause))
	switch g.kind {
	case GenOpening:
		r.failTurn(errs.CodeAiGenerationFailed, e.Cause, string(g.g.Summary().LLMProvider))
	case GenApology:
		r.cancelGeneration()
		r.completeFailedTurn(true)
	default:
		r.failReply(g, e.Cause)
	}
}

// failReply is rows 11 and 16: a REPLY generation failed (g; nil when it could not even start).
// The turn is marked FAILED and persisted when the APOLOGY ends. After the first audio the client
// first hears ai.interrupted{GENERATION_FAILED} (row 16); before it there is nothing to interrupt
// (row 11). error{AI_GENERATION_FAILED} is non-fatal and durable. The failure counts toward
// MAX_CONSECUTIVE_FAILED_TURNS unless it is CONTENT_FILTERED (S-9), and the third one in a row ends
// the session instead of apologizing (§7.11.4).
func (r *Runtime) failReply(g *genRun, cause GenCause) {
	t := r.turn
	if t == nil || t.failed != nil {
		return
	}
	var fin session.TurnFinal
	llm := ""
	if g != nil {
		llm = string(g.g.Summary().LLMProvider)
		spoken := 0
		if g.audioStarted {
			spoken = r.interruptGeneration("GENERATION_FAILED") // flushes the text, cancels, generation++
		} else {
			r.flushText(g)
			r.cancelGeneration()
		}
		fin = r.finalOf(g, session.TurnFailed, g.spokenText(spoken))
		if g.g.Text() == "" { // nothing was produced: NULL, not an empty text
			fin.AssistantText = nil
		}
		if spoken == 0 {
			fin.AssistantSpokenText = nil
		}
	} else {
		fin = session.TurnFinal{CreatedAt: t.createdAt, Status: session.TurnFailed}
	}
	code := string(errs.CodeAiGenerationFailed)
	fin.ErrorCode = &code
	t.failed = &fin
	r.turnState = StateProcessing
	r.recordTurnFailed(t, g)
	r.emitTurnError(t, g, errs.CodeAiGenerationFailed)
	if r.countFailure(cause, llm) {
		r.completeFailedTurn(true)
		r.endForFailedTurns()
		return
	}
	r.startApology()
}

// failNonJapanese is the turn whose STT text has no Japanese at all (≥ 2 runes): there is nothing
// to reply to, the turn is FAILED (error_code STT_NON_JAPANESE) and the APOLOGY asks for it again.
// It is the user's doing, so it stays outside consecutiveFailedTurns like CONTENT_FILTERED.
func (r *Runtime) failNonJapanese() {
	t := r.turn
	code := codeSTTNonJapanese
	t.failed = &session.TurnFinal{CreatedAt: t.createdAt, Status: session.TurnFailed, ErrorCode: &code}
	r.turnState = StateProcessing
	r.submitTurnFailed(t, "STT", "NON_JAPANESE")
	r.startApology()
}

// startApology plays the APOLOGY of the failed turn (§7.7.6); when it cannot start the turn ends
// at once.
func (r *Runtime) startApology() {
	if err := r.startGeneration(GenApology); err != nil {
		r.lg().Error("start apology failed", "error", err.Error())
		r.completeFailedTurn(true)
		return
	}
	r.pushTurn()
	r.pushRT()
}

// failTurn is row 12 (and a failure to build the OPENING prompt): turn 0 ends FAILED without an
// APOLOGY, the client hears error{AI_GENERATION_FAILED} and turn.completed{FAILED}, and the state
// is LISTENING. The opening is not tried again.
func (r *Runtime) failTurn(code errs.Code, cause GenCause, llm string) {
	t, g := r.turn, r.gen
	if t == nil {
		return
	}
	var fin session.TurnFinal
	if g != nil {
		r.flushText(g)
		fin = r.finalOf(g, session.TurnFailed, g.spokenText(0))
		r.cancelGeneration()
	} else {
		fin = session.TurnFinal{CreatedAt: t.createdAt, Status: session.TurnFailed, CompletedAt: r.d.Clock.Now()}
	}
	c := string(code)
	fin.ErrorCode = &c
	t.failed = &fin
	r.recordTurnFailed(t, g)
	r.emitTurnError(t, g, code)
	r.completeFailedTurn(true)
	if r.countFailure(cause, llm) {
		r.endForFailedTurns()
	}
}

// recordTurnFailed writes the TURN_FAILED session event of a turn whose generation g failed (nil
// when it could not even start): the stage and the class of the error, never a text.
func (r *Runtime) recordTurnFailed(t *turnRun, g *genRun) {
	stage, class := failStageLLM, "PROMPT_BUILD"
	if g != nil {
		if s, c := g.g.FailInfo(); s != "" {
			stage, class = s, c
		}
	}
	r.submitTurnFailed(t, stage, class)
}

func (r *Runtime) submitTurnFailed(t *turnRun, stage, class string) {
	id := t.id
	r.submit(SessionEvent{Type: string(session.EventTurnFailed), TurnID: &id, Payload: map[string]any{
		"turn_seq": t.seq, "stage": stage, "error_class": class,
	}})
}

// completeFailedTurn closes the FAILED turn: persist (the final row carries error_code), count,
// turn.completed{FAILED} and, when toListening, S-8 (c) and LISTENING. The APOLOGY's own
// generation is gone (finished, failed or cut); its text is never the turn's.
func (r *Runtime) completeFailedTurn(toListening bool) {
	t := r.turn
	if t == nil || t.failed == nil {
		return
	}
	if g := r.gen; g != nil {
		r.flushText(g)
		r.dropCancel(g.cancelID)
		r.gen = nil
	}
	fin := *t.failed
	fin.CompletedAt = r.d.Clock.Now()
	r.finalize(t.id, fin)
	r.d.Metrics.Turn(string(session.TurnFailed))
	tc := protocol.TurnCompleted{Status: string(session.TurnFailed), AssistantText: fin.AssistantText, AssistantSpokenText: fin.AssistantSpokenText}
	if t.hasUser {
		u := t.userText
		tc.UserText = &u
	}
	r.emit("turn.completed", evMeta{turn: t}, tc)
	r.turn = nil
	r.dl.playback = time.Time{}
	r.sync.ClearTurn()
	r.sttAudioBase = r.sttAudioTotal()
	if toListening {
		r.commitBoundary("listening") // S-8 (c)
		r.turnState = StateListening
	}
	r.postTurn(t, session.TurnFailed, "", "")
}

// countFailure applies the counter rule of §7.6.1 to a turn that became FAILED and reports whether
// MAX_CONSECUTIVE_FAILED_TURNS was reached. CONTENT_FILTERED (S-9) neither raises nor resets
// consecutiveFailedTurns: it has its own counter and metric and does not touch the charge.
func (r *Runtime) countFailure(cause GenCause, llm string) bool {
	if cause == CauseContentFiltered {
		r.contentFilteredTurns++
		r.d.Metrics.LLMContentFiltered(llm)
		return false
	}
	r.failedTurns++
	return r.failedTurns >= r.d.Cfg.MaxConsecutiveFailedTurns
}

// endForFailedTurns ends the session PROVIDER_FAILURE after MAX_CONSECUTIVE_FAILED_TURNS failed
// turns in a row (§7.11.4).
func (r *Runtime) endForFailedTurns() {
	r.fatal(session.EndProviderFailure, errs.CodeProviderFailure,
		fmt.Errorf("conversation: %d consecutive failed turns", r.failedTurns))
}

// sttFailed ends the session for an STT error, or a stream that closed unasked (§7.7.6, §7.11.4): the router has no other vendor to
// move to, so the session ends PROVIDER_FAILURE; there is no PROVIDER_FALLBACK.
func (r *Runtime) sttFailed(ev provider.STTEvent) {
	cause := fmt.Errorf("conversation: stt error from %s", ev.Provider)
	if ev.Type == provider.STTClosed {
		cause = fmt.Errorf("conversation: stt stream of %s closed that nobody asked to close", ev.Provider)
	}
	if ev.Err != nil {
		cause = fmt.Errorf("conversation: stt error from %s: %w", ev.Provider, ev.Err)
	}
	r.fatal(session.EndProviderFailure, errs.CodeProviderFailure, cause)
}

// isNonJapanese is the text of a user turn with at least two speakable runes and no Hiragana,
// Katakana or Han (turn FAILED, not DISCARDED).
func isNonJapanese(text string) bool {
	return !HasJapanese(text) && RuneLen(prompt.NormalizeSpeakable(text)) >= 2
}
