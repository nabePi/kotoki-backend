package conversation

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/nabePi/kotoki-backend/internal/conversation/protocol"
	"github.com/nabePi/kotoki-backend/internal/platform/errs"
	"github.com/nabePi/kotoki-backend/internal/session"
)

// userEndGrace is the grace_seconds of session.ending for USER_ENDED (§7.11.3): the hard deadline
// of the end, within which the final of a user who was speaking is waited for.
const userEndGrace = 2

// endReasonSessionEnding is stt.final.end_reason of a turn the end of the session cut (§7.4.5).
const endReasonSessionEnding = "SESSION_ENDING"

// This file is the end of a session (spec §7.11): the paths that make the runtime stop. A normal end
// (endWith: user end, expiry, reconnect timeout) is ACTIVE → ENDING at once, which is where billing
// stops, then the turn in progress is given what §7.11.2/§7.11.3 allow it: nothing (cut now), or
// until the end of its audio, or until the final of the user's words, never past the grace
// deadline. Once no turn is left, finish flushes the persister (≤ 5 s on the runtime clock) and
// settles the session (Complete). A fatal end (fatal) cancels everything, fails the turn, goes
// ENDING, flushes and settles with Fail (§7.11.4).

// endRun is the end of the session in progress after BeginEnding. started is set once the tail (the
// flush and the settlement) has begun: nothing of the turn engine acts after that.
type endRun struct {
	reason  session.EndReason
	graceBy time.Time // the hard deadline of the turn still running (§7.11.2: expires_at + 10 s)
	started bool
}

// hearsUser: the session is ACTIVE, or ending and still letting a turn finish. The detector, the
// starvation watchdog and the turn engine only act then.
func (r *Runtime) hearsUser() bool {
	return r.status == session.StatusActive || (r.status == session.StatusEnding && r.end != nil && !r.end.started)
}

// onSessionEnd is session.end (§7.11.3): remembered while STARTING, ignored once ending.
func (r *Runtime) onSessionEnd() {
	switch {
	case r.status == session.StatusStarting || r.takingOver:
		r.pendingEnd = true
	case r.ending || r.status != session.StatusActive:
		// idempotent: a second session.end changes nothing
	default:
		r.userEnd()
	}
}

// userEnd ends the session at the user's request.
func (r *Runtime) userEnd() { r.endWith(session.EndUserEnded) }

// onExpiry is `expires_at` reached (§7.11.2).
func (r *Runtime) onExpiry() { r.endWith(session.EndDurationElapsed) }

// graceFor is the hard deadline of the end for reason.
func (r *Runtime) graceFor(reason session.EndReason, now time.Time) time.Time {
	switch reason {
	case session.EndDurationElapsed:
		return r.expiresAt.Add(r.d.Cfg.SessionExpiryGrace)
	case session.EndUserEnded:
		return now.Add(userEndGrace * time.Second)
	}
	return now
}

// graceSeconds is session.ending.grace_seconds: 2 for a user end, for an expiry 0 when nothing is
// running and else what is left of the grace (§7.11.2), nothing for the rest.
func (r *Runtime) graceSeconds(e *endRun, now time.Time) int {
	switch e.reason {
	case session.EndUserEnded:
		return userEndGrace
	case session.EndDurationElapsed:
		if r.turn == nil {
			return 0
		}
		return max(0, int((e.graceBy.Sub(now)+time.Second-1)/time.Second))
	}
	return 0
}

// endWith is ACTIVE → ENDING for a normal end (§7.11.2/§7.11.3): turns are no longer accepted,
// BeginEnding (billing stops here, whatever the grace takes), session.ending, then the turn in
// progress is handled by driveEnd and, when none is left, the session is finished.
func (r *Runtime) endWith(reason session.EndReason) {
	if r.ending || r.stopped {
		return
	}
	r.ending, r.accepting = true, false
	ctx, cancel := r.ioCtx()
	defer cancel()
	if !r.settleDisconnected(ctx) { // a client that is away at the end: the time away is not charged (§7.10.1)
		return
	}
	if err := r.d.Session.BeginEnding(ctx, r.sid, r.fence, reason); err != nil {
		r.endCallFailed("BeginEnding", err)
		return
	}
	now := r.d.Clock.Now()
	r.status = session.StatusEnding
	r.moveState(SessionEnding)
	e := &endRun{reason: reason, graceBy: r.graceFor(reason, now)}
	r.end = e
	r.emit("session.ending", evMeta{}, protocol.SessionEnding{
		Stage: "ENDING", Reason: string(reason), RemainingSeconds: r.remainingSeconds(), GraceSeconds: r.graceSeconds(e, now),
	})
	r.pushRT()
	r.dl.grace = e.graceBy
	r.driveEnd(now)
	r.advanceEnd()
}

// driveEnd gives the turn in progress what the end allows (§7.11.2 for an expiry, §7.11.3 for a
// user end and every other normal end). An expiry lets the turn run to its natural end within the
// grace: the user's speech until the detector ends it, the AI's sentences until they have played;
// everything else is cut now.
func (r *Runtime) driveEnd(now time.Time) {
	t, g := r.turn, r.gen
	if t == nil {
		return // LISTENING: advanceEnd finishes
	}
	expiry := r.end.reason == session.EndDurationElapsed
	switch {
	case t.failed != nil || (g != nil && g.kind == GenApology):
		// The APOLOGY of a FAILED turn: short and fixed, so an expiry lets it play out; any other end
		// cuts it and the turn stays FAILED.
		if !expiry {
			r.cutTurnForEnd(now)
		}
	case t.waitingFinal:
		r.dl.sttFinal = now.Add(r.d.STTFinalTimeout) // the grace deadline cuts it anyway if that comes first
	case r.turnState == StateUserSpeaking:
		if !expiry { // the detector goes on for an expiry; a user end finalizes at once
			r.forceEndUser(now, min(r.d.STTFinalTimeout, r.end.graceBy.Sub(now)))
		}
	case g != nil && g.audioStarted && expiry:
		// AI_SPEAKING: no new sentence, the parsed ones are still synthesized and played, and the turn
		// closes when the playback is over (onPlaybackFinished / onPlaybackDeadline).
		g.g.LLMCancel()
	default:
		r.cutTurnForEnd(now)
	}
}

// onGraceDeadline is `expires_at + 10 s` (or the 2 s of a user end): whatever still runs is cut.
func (r *Runtime) onGraceDeadline() {
	r.cutTurnForEnd(r.d.Clock.Now())
	r.advanceEnd()
}

// advanceEnd finishes the session once the end has no turn left to wait for. The loop calls it after
// every input it handles, so the close of the last turn (playback over, final received, turn cut)
// leads straight to the settlement.
func (r *Runtime) advanceEnd() {
	if e := r.end; e == nil || e.started || r.stopped || r.turn != nil {
		return
	}
	r.finish()
}

// forceEndUser ends the user turn that is being spoken because the session ends (stt.final
// end_reason SESSION_ENDING): the STT is finalized and its final waited for up to wait (never
// past the grace); with no time to wait the turn goes on with the text it has.
func (r *Runtime) forceEndUser(now time.Time, wait time.Duration) {
	t := r.turn
	if t == nil || r.turnState != StateUserSpeaking {
		return
	}
	t.endReason, t.endAt, t.userEndedAt, t.noMerge = endReasonSessionEnding, now, now, true
	r.turnState = StateProcessing
	t.waitingFinal = true
	if wait > 0 && r.stt != nil {
		err := r.stt.Finalize()
		if err == nil {
			t.commit = r.sttSt.Commit(CommitTurnEnd)
			t.finalizeAt = now
			r.dl.sttFinal = now.Add(wait)
			r.pushTurn()
			return
		}
		r.lg().Warn("stt finalize failed at the end of the session: using the running text", "error", err.Error())
	}
	r.resolveUserText(true, false)
}

// cutTurnForEnd closes the turn in progress now, with what it has (§7.11.2/§7.11.3 and the grace
// deadline): the user's words are kept (COMPLETED, no answer), an AI turn that was heard is
// INTERRUPTED with what was spoken, a turn that was never heard has no AI side, and a FAILED turn
// stays FAILED. A running generation is cancelled with ai.interrupted{SESSION_ENDING}.
func (r *Runtime) cutTurnForEnd(now time.Time) {
	t, g := r.turn, r.gen
	if t == nil {
		return
	}
	switch {
	case t.failed != nil: // the APOLOGY is cut by the end: the turn stays FAILED
		if g != nil {
			r.interruptGeneration("SESSION_ENDING")
		}
		r.completeFailedTurn(false)
	case g != nil:
		heard := g.audioStarted
		spokenCount := r.interruptGeneration("SESSION_ENDING")
		switch {
		case heard || t.initiator == initiatorAI: // the AI side was heard, or turn 0 has nothing else
			r.interruptedForEnd(t, g, spokenCount)
		default: // the reply never played: the turn is the user's words alone
			r.completeUserOnly(t)
		}
	case t.waitingFinal: // the end is not waited for any more: the turn goes on with the running text
		r.dl.sttFinal = time.Time{}
		r.resolveUserText(true, false)
	case r.turnState == StateUserSpeaking:
		r.forceEndUser(now, 0)
	}
}

// interruptedForEnd finalizes the turn whose speaking the end cut: INTERRUPTED with the sentences
// the user heard.
func (r *Runtime) interruptedForEnd(t *turnRun, g *genRun, spokenCount int) {
	spoken := g.spokenText(spokenCount)
	r.finalize(t.id, r.finalOf(g, session.TurnInterrupted, spoken))
	r.d.Metrics.Turn(string(session.TurnInterrupted))
	text := g.g.Text()
	tc := protocol.TurnCompleted{Status: string(session.TurnInterrupted), AssistantText: &text, AssistantSpokenText: &spoken}
	if t.hasUser {
		u := t.userText
		tc.UserText = &u
	}
	r.emit("turn.completed", evMeta{turn: t}, tc)
	r.turn = nil
	r.sync.ClearTurn()
}

// completeUserOnly finalizes a user turn the end left without an answer: COMPLETED with the user's
// text and no assistant side (§7.11.2 PROCESSING / USER_SPEAKING).
func (r *Runtime) completeUserOnly(t *turnRun) {
	r.finalize(t.id, TurnFinal{CreatedAt: t.createdAt, Status: session.TurnCompleted, CompletedAt: r.d.Clock.Now()})
	r.d.Metrics.Turn(string(session.TurnCompleted))
	tc := protocol.TurnCompleted{Status: string(session.TurnCompleted)}
	if t.hasUser {
		u := t.userText
		tc.UserText = &u
	}
	r.emit("turn.completed", evMeta{turn: t}, tc)
	r.turn = nil
	r.sync.ClearTurn()
}

// finish is the common tail of a normal end (§7.11.2): the persister is flushed (≤ 5 s on the
// runtime clock), then the session is settled (Complete), session.completed goes out, the redis
// state is closed and the connection is closed 1000. The runtime stops after it.
func (r *Runtime) finish() { r.flushThen(r.completeAfterFlush) }

// flushThen starts the tail of the end: nothing of the turn engine runs after this, the persister
// is flushed on its own goroutine under a deadline of the runtime clock (so the loop is never
// blocked and a test moves it with the fake clock), and next runs on the loop with the result.
func (r *Runtime) flushThen(next func(error)) {
	r.end.started = true
	r.dl.grace, r.dl.sttFinal, r.dl.playback = time.Time{}, time.Time{}, time.Time{}
	r.settleBargeIn(r.d.Clock.Now()) // the BARGE_IN row of a barge-in this close to the end is still written
	if r.syncEnd {                   // the loop is dying (a panic): nobody is left to receive the result
		ctx, cancel := r.ioCtx()
		defer cancel()
		next(r.persister.Flush(ctx))
		return
	}
	ctx, cancel := r.ctxWithDeadline(r.ctx, ioTimeout)
	r.goSafe("session end flush", func() {
		err := r.persister.Flush(ctx)
		cancel(nil)
		r.post(r.ctx, func(*Runtime) { next(err) }) //nolint:contextcheck // the settlement runs on the loop under its own bound (ioCtx), as the other terminal calls do
	})
}

// flushed reports what the flush before a settlement came to; false when the runtime must stop
// instead (a lost fence). A flush that failed or timed out settles anyway: the session is over, the
// settlement must not wait for a database that does not answer (§7.11.2: ≤ 5 s).
func (r *Runtime) flushed(err error) bool {
	if r.stopped {
		return false
	}
	if errors.Is(err, session.ErrFenceLost) {
		r.onFenceLost(err)
		return false
	}
	if err != nil {
		r.lg().Error("persister flush before settlement failed", "error", err.Error())
	}
	return true
}

// completeAfterFlush is the settlement of a normal end.
func (r *Runtime) completeAfterFlush(flushErr error) {
	if !r.flushed(flushErr) {
		return
	}
	ctx, cancel := r.ioCtx()
	defer cancel()
	st, err := r.d.Session.Complete(ctx, r.sid, r.fence)
	if err != nil {
		r.endCallFailed("Complete", err)
		return
	}
	r.status = session.StatusCompleted
	r.emit("session.completed", evMeta{}, protocol.SessionCompleted{
		EndReason: string(st.EndReason), StartedAt: protocol.Timestamp{Time: st.StartedAt}, EndedAt: protocol.Timestamp{Time: st.EndedAt},
		ChargedSeconds: st.ChargedSeconds, DisconnectedSeconds: st.DisconnectedSeconds, TurnCount: r.turnCount,
	})
	r.pushRT()
	r.closeConn(1000, "")
	r.stopped = true
}

// closeConn closes the connection after the queued events were written.
func (r *Runtime) closeConn(code int, reason string) {
	if r.cs != nil {
		r.cs.closeAfterDrain(code, reason)
	}
}

// failStart is §7.6.3 step 5: the runtime was not ready in time (or a step failed), so the session
// goes STARTING → FAILED with nothing charged, and the client hears session.failed and the error.
func (r *Runtime) failStart(reason session.EndReason, code errs.Code, cause error) {
	if r.ending || r.stopped {
		return
	}
	r.ending = true
	r.lg().Error("session start failed", "reason", string(reason), "code", string(code), "error", cause.Error())
	ctx, cancel := r.ioCtx()
	defer cancel()
	st, err := r.d.Session.FailStart(ctx, r.sid, r.fence, reason)
	if err != nil {
		if errors.Is(err, session.ErrFenceLost) {
			r.onFenceLost(err)
			return
		}
		r.lg().Error("FailStart failed", "error", err.Error()) // the sweeper settles a session stuck in STARTING
	}
	r.status = session.StatusFailed
	r.emit("session.failed", evMeta{}, protocol.SessionFailed{EndReason: string(reason), ErrorCode: string(code), ChargedSeconds: st.ChargedSeconds})
	r.emitError(code, nil, nil)
	r.pushRT()
	r.closeConn(protocol.CloseCode(code), string(code))
	r.stopped = true
}

// fatal ends an ACTIVE or ENDING session with a failure (§7.11.4): turns stop, everything is
// cancelled, the turn in progress is FAILED, BeginEnding(reason), the persister is flushed (≤ 5 s),
// Fail(reason), then session.failed and the error go out and the connection closes with the code of
// the error. A fatal error while the tail of an end is already under way changes nothing: that end
// settles the session.
func (r *Runtime) fatal(reason session.EndReason, code errs.Code, cause error) {
	if r.stopped {
		return
	}
	if r.status == session.StatusStarting {
		r.failStart(reason, code, cause)
		return
	}
	if (r.ending && r.status != session.StatusEnding) || (r.end != nil && r.end.started) {
		return
	}
	r.lg().Error("session failed", "reason", string(reason), "code", string(code), "error", cause.Error())
	r.ending, r.accepting = true, false
	r.dl.grace, r.dl.sttFinal, r.dl.playback = time.Time{}, time.Time{}, time.Time{}
	r.cancelGeneration()
	r.failTurnSilently()
	ctx, cancel := r.ioCtx()
	defer cancel()
	if !r.settleDisconnected(ctx) {
		return
	}
	if err := r.d.Session.BeginEnding(ctx, r.sid, r.fence, reason); err != nil {
		r.endCallFailed("BeginEnding", err)
		return
	}
	r.status = session.StatusEnding
	r.moveState(SessionEnding)
	r.end = &endRun{reason: reason}
	r.emit("session.ending", evMeta{}, protocol.SessionEnding{
		Stage: "ENDING", Reason: string(reason), RemainingSeconds: r.remainingSeconds(),
	})
	r.flushThen(func(err error) { r.failAfterFlush(err, reason, code) })
}

// failAfterFlush is the settlement of a fatal end.
func (r *Runtime) failAfterFlush(flushErr error, reason session.EndReason, code errs.Code) {
	if !r.flushed(flushErr) {
		return
	}
	ctx, cancel := r.ioCtx()
	defer cancel()
	st, err := r.d.Session.Fail(ctx, r.sid, r.fence, reason)
	if err != nil {
		r.endCallFailed("Fail", err)
		return
	}
	r.status = session.StatusFailed
	r.emit("session.failed", evMeta{}, protocol.SessionFailed{EndReason: string(reason), ErrorCode: string(code), ChargedSeconds: st.ChargedSeconds})
	r.emitError(code, nil, nil)
	r.pushRT()
	r.closeConn(protocol.CloseCode(code), string(code))
	r.stopped = true
}

// failTurnSilently marks the turn in progress FAILED for a session that is about to end: no event
// reaches the client (session.failed says it all).
func (r *Runtime) failTurnSilently() {
	t, g := r.turn, r.gen
	if t == nil {
		return
	}
	var fin session.TurnFinal
	switch {
	case t.failed != nil: // already FAILED (waiting for its APOLOGY): keep its own row and error code
		fin = *t.failed
		fin.CompletedAt = r.d.Clock.Now()
	case g != nil:
		fin = r.finalOf(g, session.TurnFailed, g.spokenText(0))
	default:
		fin = session.TurnFinal{CreatedAt: t.createdAt, Status: session.TurnFailed, CompletedAt: r.d.Clock.Now()}
	}
	if fin.ErrorCode == nil {
		c := string(errs.CodeInternalError)
		fin.ErrorCode = &c
	}
	r.finalize(t.id, fin)
	r.d.Metrics.Turn(string(session.TurnFailed))
	r.turn = nil
	r.sync.ClearTurn()
}

// endCallFailed handles an error of a transition call the loop made while ending: a lost fence
// stops the runtime without writing; anything else leaves the session for the sweeper (the status
// is not terminal, §5.6) and closes the connection with 1011.
func (r *Runtime) endCallFailed(call string, err error) {
	if errors.Is(err, session.ErrFenceLost) {
		r.onFenceLost(err)
		return
	}
	r.lg().Error("session transition failed", "call", call, "error", err.Error())
	r.emitError(errs.CodeInternalError, nil, nil)
	r.closeConn(protocol.CloseCode(errs.CodeInternalError), string(errs.CodeInternalError))
	r.stopped = true
}

// onFenceLost is §7.9.3: a newer owner has the session (ErrFenceLost, a refused owner key or a
// TAKEOVER message). The runtime stops without writing anything else. The client is told 4011
// SESSION_TAKEN_OVER, or 4010 SESSION_NOT_RESUMABLE when the session is already in a terminal status
// (the sweeper settled it): only that read is made.
func (r *Runtime) onFenceLost(err error) {
	if r.stopped {
		return
	}
	r.lg().Warn("fence lost: stopping without further writes", "error", err.Error())
	r.latch.trip() // fence_lost_total once, whichever way the runtime found out (persister, owner key, TAKEOVER)
	r.persister.Abort()
	r.sync.Abort()
	r.cancelGeneration()
	if r.cs != nil {
		code := errs.CodeSessionTakenOver
		if !errors.Is(err, errTakenOver) && r.sessionIsTerminal() {
			code = errs.CodeSessionNotResumable
		}
		r.emitError(code, nil, nil)
		r.closeConn(protocol.CloseCode(code), string(code))
	}
	r.stopped = true
}

// sessionIsTerminal reads the status of the session: true when it is COMPLETED, FAILED, CANCELLED or
// ABANDONED. A read that fails says nothing, and the runtime then assumes a takeover.
func (r *Runtime) sessionIsTerminal() bool {
	ctx, cancel := r.ioCtx()
	defer cancel()
	rec, err := r.d.Session.GetForAttach(ctx, r.sid)
	return err == nil && rec.Status.Terminal()
}

// onFatalSignal is a SERVER_ERROR reported from another goroutine: the persister (stuck for 30 s,
// or a rejected job) or a recovered panic (§7.11.4).
func (r *Runtime) onFatalSignal(err error) {
	var pe *panicError
	if errors.As(err, &pe) {
		r.fatalPanic(pe)
		return
	}
	r.fatal(session.EndServerError, errs.CodeInternalError, fmt.Errorf("conversation: persister: %w", err))
}

// onHardStop is the pod context ending (process shutdown). The graceful path is the drain of
// Task 23; this one only stops: the connection is told to go elsewhere, the session stays ACTIVE
// for a resume or the sweeper.
func (r *Runtime) onHardStop() {
	if r.cs != nil {
		r.emitError(errs.CodeServerDraining, nil, nil)
		r.cs.closeNow(protocol.CloseCode(errs.CodeServerDraining), string(errs.CodeServerDraining))
	}
	r.stopped = true
}

// exit releases everything the runtime holds, in the one place that does: the persister is
// drained first (its worker still runs), then the redisSync writes its last state and releases the
// owner key, then the goroutines are stopped and awaited, and the Manager forgets the runtime.
func (r *Runtime) exit() {
	if r.gen != nil {
		r.gen.g.Cancel()
	}
	ctx, cancel := r.ioCtx()
	if err := r.persister.Close(ctx); err != nil && !errors.Is(err, session.ErrFenceLost) && !errors.Is(err, context.Canceled) {
		r.lg().Warn("persister close", "error", err.Error())
	}
	if !r.syncReleased { // the handoff of a drain released it already, before the client was told
		if err := r.sync.Release(ctx); err != nil && !errors.Is(err, session.ErrFenceLost) {
			r.lg().Warn("redis sync release", "error", err.Error())
		}
	}
	cancel()
	r.cancel()
	if r.gen != nil {
		r.gen.g.Wait()
	}
	for _, g := range r.retired {
		g.Wait()
	}
	r.closeSTT(r.stt, r.sttCancel)
	r.wg.Wait()
	if r.cs != nil {
		r.cs.closeNow(protocol.CloseCode(errs.CodeServerDraining), string(errs.CodeServerDraining)) // no-op when a close is under way
	}
	r.d.Metrics.SessionState(r.metricState, "")
	if r.onExit != nil {
		r.onExit(r)
	}
	close(r.doneCh)
}
