package conversation

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/nabePi/kotoki-backend/internal/conversation/protocol"
	"github.com/nabePi/kotoki-backend/internal/conversation/rtstate"
	"github.com/nabePi/kotoki-backend/internal/platform/errs"
	"github.com/nabePi/kotoki-backend/internal/session"
)

// defaultPodSubscribeBackoff is the wait before the TAKEOVER subscriber tries again (§7.9.4).
const defaultPodSubscribeBackoff = time.Second

// ---- resume on the pod that holds the runtime (§7.10.2) ----

// resumeSamePod gives the new connection to the runtime that holds the session. The STT stream is
// opened here, off the loop (connecting takes up to 3 s and the loop must keep serving timers);
// the loop then swaps the connection in (Runtime.onAttach).
func (m *Manager) resumeSamePod(ctx context.Context, h *handshake, r *Runtime, lastSeq int64) error {
	var snap ConfigSnapshot
	var ok bool
	if err := r.call(ctx, func(r *Runtime) { ok, snap = r.resumable(), r.snap }); err != nil || !ok {
		return m.notResumableOrDraining(ctx, h)
	}
	a := &attachReq{h: h, lastSeq: lastSeq}
	a.stt, a.cancel, a.sttErr = r.openSTT(ctx, snap)
	if a.sttErr != nil && ctx.Err() != nil { // the client gave up while the stream was opening
		return ctx.Err()
	}
	if err := r.call(ctx, func(r *Runtime) { r.onAttach(a) }); err != nil { //nolint:contextcheck // the loop runs it on the runtime context; the request context only bounds the wait
		r.closeSTT(a.stt, a.cancel) // the runtime stopped, or the client went, before it took the connection
		return m.notResumableOrDraining(ctx, h)
	}
	if a.err != nil {
		return m.notResumableOrDraining(ctx, h)
	}
	return nil
}

// notResumableOrDraining refuses a resume that found its runtime gone or not taking connections: on
// a draining pod that is the handoff of the drain (the session is still ACTIVE: 4100, the client
// fetches a new ticket and resumes on another pod), otherwise the session is not resumable.
func (m *Manager) notResumableOrDraining(ctx context.Context, h *handshake) error {
	if m.Draining() {
		if sess, err := m.d.Session.GetForAttach(ctx, h.sid); err == nil && sess.Status == session.StatusActive {
			m.d.Metrics.Reconnect("rejected", "", 0)
			m.d.Metrics.Connection(ConnRejectedDraining)
			return h.fail(errs.CodeServerDraining)
		}
	}
	return m.notResumable(h)
}

// notResumable refuses a resume: ENDING and terminal sessions take no connection (§7.3.2).
func (m *Manager) notResumable(h *handshake) error {
	m.d.Metrics.Reconnect("rejected", "", 0)
	m.d.Metrics.Connection(ConnRejectedState)
	return h.fail(errs.CodeSessionNotResumable)
}

// ---- takeover (§7.10.3) ----

// takeover resumes a session on this pod that another pod owned (or that nobody holds any more).
// Steps 2–5 run here: the claim raises the fence (which is what stops the previous owner), the
// previous owner is told, what it left in Redis is read, and a client that stayed away longer than
// RECONNECT_WINDOW ends the session instead. Otherwise a Runtime is built that plays steps 6–11.
func (m *Manager) takeover(ctx context.Context, h *handshake, rec session.RuntimeRecord, lastSeq int64) error {
	if code := m.reserveUpTo(m.d.Limits.HardMax); code != "" {
		return m.refuseSlot(h, code)
	}
	fence, claimed, err := m.d.Session.ClaimRuntime(ctx, rec.ID, []session.Status{session.StatusActive})
	if err != nil {
		m.unreserve()
		return m.takeoverRefused(h, err)
	}
	in := m.readTakeover(ctx, rec.ID, fence, lastSeq)
	now := m.d.Clock.Now()
	if now.Sub(in.from) > m.d.Cfg.ReconnectWindow { // step 5: the client was away too long
		m.unreserve()
		return m.takeoverExpired(ctx, h, claimed, fence, in, now)
	}
	// The runtime outlives the connection and the request: its context is the pod's.
	r := newRuntime(m.ctx, m.d, claimed, fence, h.cs, h.seen, m.onRuntimeExit, RedisSyncLastDurableSeq(in.lastDurable)) //nolint:contextcheck // runtime context = pod context
	r.serverSeq = in.known                                                                                              // the next event continues the previous owner's numbering
	draining := m.register(r)
	m.d.Metrics.Connection(ConnAccepted)
	r.beginTakeover(in) //nolint:contextcheck // the runtime goroutines run on the runtime context (pod context)
	if draining {
		m.tellDrain(r)
	}
	return nil
}

// takeoverRefused maps an error of ClaimRuntime: a session that is not ACTIVE any more (or gone) is
// not resumable; a fence that moved again means yet another owner took it.
func (m *Manager) takeoverRefused(h *handshake, err error) error {
	switch {
	case errors.Is(err, session.ErrNotFound), errors.Is(err, session.ErrInvalidTransition):
		return m.notResumable(h)
	case errors.Is(err, session.ErrFenceLost):
		m.d.Metrics.Reconnect("rejected", "", 0)
		m.d.Metrics.Connection(ConnRejectedState)
		return h.fail(errs.CodeSessionTakenOver)
	}
	m.d.Log.Error("takeover refused", "session_id", h.sid.String(), "error", err.Error())
	m.d.Metrics.Connection(ConnRejectedUnavailable)
	return h.fail(errs.CodeServiceUnavailable)
}

// readTakeover is steps 3–5: it reads what the previous owner left in Redis, tells that owner to
// stop and works out when the client was last heard. Every Redis call is bounded to 200 ms; when
// one fails Redis is treated as down (§7.9.4): the takeover runs on Postgres alone, the time away
// is not deducted and the replay is unavailable.
func (m *Manager) readTakeover(ctx context.Context, sid uuid.UUID, fence, lastSeq int64) *takeoverInput {
	in := &takeoverInput{lastSeq: lastSeq}
	now := m.d.Clock.Now()
	st := m.d.Store
	var (
		wg     sync.WaitGroup
		mu     sync.Mutex
		failed bool
		hb     time.Time
		haveHB bool
	)
	run := func(f func(ctx context.Context) error) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cctx, cancel := context.WithTimeout(ctx, redisOpTimeout)
			defer cancel()
			if err := f(cctx); err != nil {
				mu.Lock()
				failed = true
				mu.Unlock()
				m.d.Log.Warn("takeover: redis read failed", "error", err.Error())
			}
		}()
	}
	run(func(c context.Context) (err error) { in.rt, in.haveRT, err = st.ReadRT(c, sid); return err })
	run(func(c context.Context) (err error) { in.turn, in.haveTurn, err = st.ReadTurn(c, sid); return err })
	run(func(c context.Context) (err error) {
		in.summary, in.haveSummary, err = st.ReadSummary(c, sid)
		return err
	})
	run(func(c context.Context) (err error) { in.entries, err = st.ReadEvents(c, sid); return err })
	run(func(c context.Context) (err error) { hb, haveHB, err = st.ReadHeartbeat(c, sid); return err })
	wg.Wait()
	if failed {
		*in = takeoverInput{lastSeq: lastSeq, down: true, from: now, known: lastSeq, lastDurable: lastSeq}
		return in
	}
	if in.haveRT {
		in.fromPod = in.rt.PodID
		pctx, cancel := context.WithTimeout(ctx, redisOpTimeout) // step 3: the previous owner stops (the fence already stops its writes)
		if err := st.PublishTakeover(pctx, in.rt.PodID, rtstate.Takeover{SessionID: sid, Fence: fence}); err != nil {
			m.d.Log.Warn("takeover: publish failed", "pod", in.rt.PodID, "error", err.Error())
		}
		cancel()
	}
	switch { // step 5: disconnected_at, else the heartbeat, else the last rt write
	case in.haveRT && in.rt.DisconnectedAtMS > 0:
		in.from = time.UnixMilli(in.rt.DisconnectedAtMS).UTC()
	case haveHB:
		in.from = hb
	case in.haveRT && in.rt.UpdatedAtMS > 0:
		in.from = time.UnixMilli(in.rt.UpdatedAtMS).UTC()
	default:
		in.from = now
	}
	in.known, in.lastDurable = in.rt.LastServerSeq, in.rt.LastDurableSeq
	if n := len(in.entries); n > 0 {
		tip := in.entries[n-1].Seq
		in.known, in.lastDurable = max(in.known, tip), max(in.lastDurable, tip)
	}
	return in
}

// takeoverExpired is step 5 when the client has been away longer than RECONNECT_WINDOW: the time
// away is added, the session ends RECONNECT_TIMEOUT and the connection that asked for the resume
// is told and closed 1000. No Runtime is built.
func (m *Manager) takeoverExpired(ctx context.Context, h *handshake, rec session.RuntimeRecord, fence int64, in *takeoverInput, now time.Time) error {
	reject := func(err error) error {
		if errors.Is(err, session.ErrFenceLost) {
			return m.takeoverRefused(h, err)
		}
		m.d.Log.Error("takeover: ending a timed-out session failed", "session_id", h.sid.String(), "error", err.Error())
		m.d.Metrics.Connection(ConnRejectedUnavailable)
		return h.fail(errs.CodeServiceUnavailable)
	}
	sid := rec.ID
	if err := failInProgressTurns(ctx, m.d, fence, sid, in, now); err != nil { // a turn left without a status must not stay that way
		return reject(err)
	}
	if secs := int(now.Sub(in.from) / time.Second); secs > 0 {
		if err := m.d.Session.AddDisconnected(ctx, sid, fence, secs); err != nil {
			return reject(err)
		}
	}
	if err := m.d.Session.BeginEnding(ctx, sid, fence, session.EndReconnectTimeout); err != nil {
		return reject(err)
	}
	st, err := m.d.Session.Complete(ctx, sid, fence)
	if err != nil {
		return reject(err)
	}
	turns := 0
	if recent, err := m.d.Turns.ListRecent(ctx, sid, maxTakeoverTurns); err == nil {
		turns = len(recent)
	}
	h.seq = max(h.seq, in.known) // the events below continue the numbering the client has seen
	h.send("session.ending", protocol.SessionEnding{Stage: "ENDING", Reason: string(session.EndReconnectTimeout)})
	h.send("session.completed", protocol.SessionCompleted{
		EndReason: string(st.EndReason), StartedAt: protocol.Timestamp{Time: st.StartedAt}, EndedAt: protocol.Timestamp{Time: st.EndedAt},
		ChargedSeconds: st.ChargedSeconds, DisconnectedSeconds: st.DisconnectedSeconds, TurnCount: turns,
	})
	h.cs.closeAfterDrain(1000, "")
	m.d.Metrics.Reconnect("timeout", "", now.Sub(in.from))
	return nil
}

// failInProgressTurns is step 6: every turn the previous owner left without a status becomes FAILED
// with what Redis kept of it (D-084), and TURN_FAILED{stage:TAKEOVER} records the turn Redis knew.
func failInProgressTurns(ctx context.Context, d Deps, fence int64, sid uuid.UUID, in *takeoverInput, now time.Time) error {
	var partial *session.TurnFinal
	if in.haveTurn {
		fin := session.TurnFinal{ErrorCode: nonEmpty(classTakeover), CompletedAt: now}
		fin.AssistantText = nonEmpty(in.turn.AssistantText)
		partial = &fin
	}
	n, err := d.Turns.FailInProgress(ctx, fence, sid, partial)
	if err != nil {
		return fmt.Errorf("conversation: fail in-progress turns: %w", err)
	}
	if n > 0 && in.haveTurn {
		id := in.turn.TurnID
		if err := d.Events.Append(ctx, fence, sid, string(session.EventTurnFailed), &id, map[string]any{
			"turn_seq": in.turn.TurnSeq, "stage": stageTakeover, "error_class": classTakeover,
		}); err != nil {
			return fmt.Errorf("conversation: append TURN_FAILED: %w", err)
		}
	}
	return nil
}

// ---- TAKEOVER subscriber (§7.9.1, §7.9.4) ----

// StartSubscriber starts the subscriber of the pod's TAKEOVER channel; it runs until ctx ends or
// the Manager is closed, and only the first call counts. The first subscription is made here,
// before it returns, so a takeover published afterwards is not lost; the goroutine resubscribes
// (backoff 1 s) when the subscription ends or fails (§7.9.4: the fence in Postgres keeps a takeover
// safe while the channel is down).
func (m *Manager) StartSubscriber(ctx context.Context) {
	if m.d.Store == nil {
		return
	}
	m.subOnce.Do(func() {
		sctx, cancel := context.WithCancel(ctx)
		stop := context.AfterFunc(m.ctx, cancel) //nolint:contextcheck // the subscription also ends with the Manager (the pod context), not only with the caller's ctx
		ch := m.subscribeOnce(sctx)
		m.bg.Add(1)
		go func() {
			defer m.bg.Done()
			defer stop()
			defer cancel()
			for {
				if ch != nil {
					select {
					case t, ok := <-ch:
						if ok {
							m.onTakeover(t) //nolint:contextcheck // posted to the runtime loop, which reads the session status under its own bound (ioCtx)
							continue
						}
					case <-sctx.Done():
						return
					}
				}
				timer := time.NewTimer(m.d.PodSubscribeBackoff)
				select {
				case <-timer.C:
				case <-sctx.Done():
					timer.Stop()
					return
				}
				ch = m.subscribeOnce(sctx)
			}
		}()
	})
}

func (m *Manager) subscribeOnce(ctx context.Context) <-chan rtstate.Takeover {
	ch, err := m.d.Store.SubscribePod(ctx, m.d.Cfg.PodID)
	if err != nil {
		m.d.Log.Warn("subscribe to the pod channel failed", "pod", m.d.Cfg.PodID, "error", err.Error())
		return nil
	}
	return ch
}

// onTakeover stops the local runtime of a session another pod took over: its fence is older than
// the message's. The fence in Postgres is what makes this safe even when the message is lost.
func (m *Manager) onTakeover(t rtstate.Takeover) {
	r, ok := m.Runtime(t.SessionID)
	if !ok {
		return
	}
	r.post(m.ctx, func(r *Runtime) {
		if t.Fence > r.fence {
			r.onFenceLost(errTakenOver)
		}
	})
}
