package conversation

import (
	"context"
	"errors"
	"time"

	"github.com/nabePi/kotoki-backend/internal/conversation/protocol"
	"github.com/nabePi/kotoki-backend/internal/platform/errs"
	"github.com/nabePi/kotoki-backend/internal/session"
)

// classDrain is the error_class of the turn a drain cuts at the safepoint timeout (§7.12.1). The
// turn fails because its connection is being handed to another pod, so TURN_FAILED carries the
// stage the catalog has for that (DISCONNECT) and this class tells it apart from a lost client.
const classDrain = "SERVER_DRAINING"

// ---- the pod (§7.12.1) ----

// Drain is SIGTERM: the pod stops admitting (Admission, Ready and every start or takeover that
// reaches Attach refuse from now on), podinfo says draining, and every runtime is told. Each
// runtime then does what its state asks (Runtime.onDrain): a start finishes first, an ENDING
// session finishes normally, a detached one is handed off at once and a connected one at the first
// safepoint (LISTENING with no turn running), or after DRAIN_SAFEPOINT_TIMEOUT. Drain returns when
// the pod holds no session any more, or with ctx's error when ctx ends first; the caller then
// closes the Manager (the hard stop) and what is left is the sweeper's.
//
// A second call tells the runtimes again (it changes nothing) and waits as well.
func (m *Manager) Drain(ctx context.Context) error {
	start := m.d.Clock.Now()
	m.mu.Lock()
	first := !m.draining
	m.draining = true
	rts := make([]*Runtime, 0, len(m.runtimes))
	for _, r := range m.runtimes {
		rts = append(rts, r)
	}
	m.mu.Unlock()
	if first {
		m.d.Metrics.SetDraining(true)
	}
	m.writePodInfo(ctx) // draining=1 at once: the dashboards and the autoscaler see it
	for _, r := range rts {
		m.tellDrainCtx(ctx, r)
	}
	// A runtime registered after the snapshot above heard of the drain from register (Attach); the
	// slots of the starts in flight are counted too, so the wait is for the pod to be empty.
	for {
		m.mu.Lock()
		empty := len(m.runtimes)+m.pending == 0
		changed := m.changed
		m.mu.Unlock()
		if empty {
			break
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if first {
		m.d.Metrics.DrainDuration(m.d.Clock.Now().Sub(start))
	}
	return nil
}

// tellDrain sends the DRAIN control message to a runtime registered while the pod was draining.
func (m *Manager) tellDrain(r *Runtime) { m.tellDrainCtx(m.ctx, r) }

func (m *Manager) tellDrainCtx(ctx context.Context, r *Runtime) {
	r.post(ctx, func(r *Runtime) { r.onDrain() })
}

// ---- the runtime ----

// onDrain is the ctl DRAIN of §7.12.1. STARTING and takeover-in-progress runtimes finish that first
// and an ENDING one finishes normally (finish): advanceDrain acts only on an ACTIVE session, so all
// of them just remember the drain. The loop calls advanceDrain after every input, so the first
// moment the runtime can be handed off, it is.
func (r *Runtime) onDrain() {
	r.handled++
	if r.draining || r.stopped {
		return
	}
	r.draining = true
	r.lg().Info("drain: the pod is draining", "status", string(r.status), "connected", r.connected)
	r.advanceDrain()
}

// advanceDrain hands the session off as soon as it may: when it is ACTIVE, not ending and not being
// taken over, and either nobody is connected (detached: nothing to wait for) or the turn state is
// LISTENING with no turn running (the safepoint). Until then a turn may start and run; the
// safepoint deadline (DRAIN_SAFEPOINT_TIMEOUT) is armed on the first look at an ACTIVE runtime,
// counted from then.
func (r *Runtime) advanceDrain() {
	if !r.draining || r.handoff || r.stopped {
		return
	}
	if r.status != session.StatusActive || r.ending || r.takingOver {
		return
	}
	if r.dl.safepoint.IsZero() {
		r.dl.safepoint = r.d.Clock.Now().Add(r.d.Cfg.DrainSafepointTimeout)
	}
	if !r.connected || (r.turn == nil && r.gen == nil && r.turnState == StateListening) {
		r.beginHandoff()
	}
}

// onSafepointTimeout is DRAIN_SAFEPOINT_TIMEOUT: the turn still running is cut (the generation is
// cancelled, the turn is FAILED with what it has) and the session is handed off.
func (r *Runtime) onSafepointTimeout() {
	if !r.draining || r.handoff || r.stopped || r.ending {
		return
	}
	r.lg().Warn("drain: safepoint timeout, cutting the turn in progress")
	spoken := 0
	if r.gen != nil && r.cs != nil {
		spoken = r.cs.writer.SentencesWritten(r.gen.number)
	}
	r.failTurnForDetach(spoken, stageDisconnect, classDrain)
	r.resetListening()
	r.beginHandoff()
}

// beginHandoff starts the handoff of §7.12.1: no turn is accepted from now on and the persister is
// flushed (step 1) on its own goroutine under DRAIN_FLUSH_TIMEOUT of the runtime clock, so the loop
// is not blocked and a test moves the timeout with the fake clock; the rest runs on the loop once
// the flush has answered (onHandoffFlushed).
func (r *Runtime) beginHandoff() {
	r.handoff, r.accepting = true, false
	r.dl.safepoint = time.Time{}
	ctx, cancel := r.ctxWithDeadline(r.ctx, r.d.Cfg.DrainFlushTimeout)
	r.goSafe("drain flush", func() {
		err := r.persister.Flush(ctx)
		cancel(nil)
		r.post(r.ctx, func(*Runtime) { r.onHandoffFlushed(err) }) //nolint:contextcheck // the handoff runs on the loop under its own bound (ioCtx), like the end of a session
	})
}

// onHandoffFlushed is §7.12.1 steps 2–6, on the loop, after the persister was flushed (or the flush
// timed out: the handoff goes on, what was not written is for the sweeper and the takeover).
func (r *Runtime) onHandoffFlushed(flushErr error) {
	if r.stopped {
		return
	}
	if r.ending { // an end began while the flush ran (session.end, expiry): it settles the session, not the handoff
		r.handoff = false
		return
	}
	if errors.Is(flushErr, session.ErrFenceLost) {
		r.onFenceLost(flushErr)
		return
	}
	if flushErr != nil {
		r.lg().Warn("drain: persister flush before the handoff failed", "error", flushErr.Error())
	}
	ctx, cancel := r.ioCtx()
	defer cancel()

	// 2. redisSync: rt (connected=0, disconnected_at=now; a client that was away already keeps the
	// time it was lost), turn, summary, hb.
	if r.disconnectedAt.IsZero() {
		r.disconnectedAt = r.d.Clock.Now()
	}
	r.pushRT()
	if err := r.sync.Flush(ctx); err != nil {
		if errors.Is(err, session.ErrFenceLost) {
			r.onFenceLost(err)
			return
		}
		r.lg().Warn("drain: redis flush failed", "error", err.Error())
	}
	// 3. handoff_at (S-10): a failure is logged and the handoff goes on (the fence says who owns).
	if err := r.d.Session.MarkHandoff(ctx, r.sid, r.fence); err != nil {
		r.lg().Warn("drain: MarkHandoff failed", "error", err.Error())
	}
	// 4. OWNER_CHANGED{DRAIN_RELEASE}: written directly, after MarkHandoff, the persister being idle.
	if err := r.d.Events.Append(ctx, r.fence, r.sid, string(session.EventOwner), nil, ownerChanged{
		Reason: "DRAIN_RELEASE", FromPod: r.pod, ToPod: nil, Fence: r.fence,
	}); err != nil {
		r.lg().Warn("drain: OWNER_CHANGED not written", "error", err.Error())
	}
	// 5. release the owner key: another pod may take the session over without waiting for the TTL.
	if err := r.sync.Release(ctx); err != nil && !errors.Is(err, session.ErrFenceLost) {
		r.lg().Warn("drain: redis release failed", "error", err.Error())
	}
	r.syncReleased = true
	// 6. error{SERVER_DRAINING}, close 4100; the session stays ACTIVE for the pod that resumes it.
	r.emitError(errs.CodeServerDraining, nil, nil)
	r.closeConn(protocol.CloseCode(errs.CodeServerDraining), string(errs.CodeServerDraining))
	r.stopped = true
}
