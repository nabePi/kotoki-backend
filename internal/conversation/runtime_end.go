package conversation

import (
	"context"
	"errors"
	"fmt"

	"github.com/nabePi/kotoki-backend/internal/conversation/protocol"
	"github.com/nabePi/kotoki-backend/internal/platform/errs"
	"github.com/nabePi/kotoki-backend/internal/session"
)

// userEndGrace is the grace_seconds of session.ending for USER_ENDED (§7.11.3).
const userEndGrace = 2

// This file is the end of a session: the paths that make the runtime stop. Task 18 implements
// the shape every later case needs (BeginEnding → settle → events → close → exit) with the
// simplest turn handling; Task 22 refines endWith for the turn states of §7.11.2/§7.11.3
// (USER_SPEAKING and PROCESSING, the grace deadline, AI_SPEAKING finishing its sentences) and
// adds panic recovery for generation goroutines.

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
func (r *Runtime) userEnd() { r.endWith(session.EndUserEnded, userEndGrace) }

// onExpiry is `expires_at` reached (§7.11.2).
func (r *Runtime) onExpiry() { r.endWith(session.EndDurationElapsed, 0) }

// endWith is ACTIVE → ENDING → COMPLETED for a normal end (user end, expiry): BeginEnding, the
// session.ending event, the turn in progress closed, then finish.
func (r *Runtime) endWith(reason session.EndReason, graceSeconds int) {
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
	r.status = session.StatusEnding
	r.moveState(SessionEnding)
	r.emit("session.ending", evMeta{}, protocol.SessionEnding{
		Stage: "ENDING", Reason: string(reason), RemainingSeconds: r.remainingSeconds(), GraceSeconds: graceSeconds,
	})
	r.closeTurnForEnd()
	r.finish()
}

// closeTurnForEnd closes the turn in progress when the session ends: the generation is cancelled
// (ai.interrupted{SESSION_ENDING}) and the turn becomes INTERRUPTED with what was spoken.
func (r *Runtime) closeTurnForEnd() {
	t, g := r.turn, r.gen
	if t == nil || g == nil {
		return
	}
	if t.failed != nil { // the APOLOGY is cut by the end: the turn stays FAILED
		r.interruptGeneration("SESSION_ENDING")
		r.completeFailedTurn(false)
		return
	}
	spokenCount := r.interruptGeneration("SESSION_ENDING")
	spoken := g.spokenText(spokenCount)
	r.finalize(t.id, r.finalOf(g, session.TurnInterrupted, spoken))
	r.d.Metrics.Turn(string(session.TurnInterrupted))
	text := g.g.Text()
	r.emit("turn.completed", evMeta{turn: t}, protocol.TurnCompleted{
		Status: string(session.TurnInterrupted), AssistantText: &text, AssistantSpokenText: &spoken,
	})
	r.turn = nil
	r.sync.ClearTurn()
}

// finish is the common tail of a normal end (§7.11.2): the persister is flushed (≤ 5 s), the
// session is settled (Complete), session.completed goes out, the redis state is closed and the
// connection is closed 1000. The runtime stops after it.
func (r *Runtime) finish() {
	ctx, cancel := r.ioCtx()
	defer cancel()
	if err := r.persister.Flush(ctx); err != nil {
		if errors.Is(err, session.ErrFenceLost) {
			r.onFenceLost(err)
			return
		}
		r.lg().Error("persister flush before settlement failed", "error", err.Error())
	}
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

// fatal ends an ACTIVE or ENDING session with a failure (§7.11.4): turns stop, the turn in
// progress is FAILED, BeginEnding(reason), the persister is flushed (≤ 5 s), Fail(reason), then
// session.failed and the error go out and the connection closes with the code of the error.
func (r *Runtime) fatal(reason session.EndReason, code errs.Code, cause error) {
	if r.stopped {
		return
	}
	if r.status == session.StatusStarting {
		r.failStart(reason, code, cause)
		return
	}
	if r.ending && r.status != session.StatusEnding {
		return
	}
	r.lg().Error("session failed", "reason", string(reason), "code", string(code), "error", cause.Error())
	r.ending, r.accepting = true, false
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
	if err := r.persister.Flush(ctx); err != nil && errors.Is(err, session.ErrFenceLost) {
		r.onFenceLost(err)
		return
	}
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
// TAKEOVER message). The runtime stops without writing anything else.
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
		r.emitError(errs.CodeSessionTakenOver, nil, nil)
		r.closeConn(protocol.CloseCode(errs.CodeSessionTakenOver), string(errs.CodeSessionTakenOver))
	}
	r.stopped = true
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
	if err := r.sync.Release(ctx); err != nil && !errors.Is(err, session.ErrFenceLost) {
		r.lg().Warn("redis sync release", "error", err.Error())
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
