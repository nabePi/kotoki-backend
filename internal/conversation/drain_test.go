package conversation_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/conversation"
	"github.com/nabePi/kotoki-backend/internal/conversation/protocol"
	"github.com/nabePi/kotoki-backend/internal/platform/errs"
	"github.com/nabePi/kotoki-backend/internal/session"
	"github.com/nabePi/kotoki-backend/internal/testutil/rtharness"
)

// ---- helpers ----

// drainAsync runs Manager.Drain on its own goroutine; wait for its end with drained.
func drainAsync(t *testing.T, m *conversation.Manager) <-chan error {
	t.Helper()
	ch := make(chan error, 1)
	go func() { ch <- m.Drain(t.Context()) }()
	return ch
}

func drained(t *testing.T, ch <-chan error) {
	t.Helper()
	select {
	case err := <-ch:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("Drain did not return")
	}
}

func stillDraining(t *testing.T, ch <-chan error) {
	t.Helper()
	select {
	case err := <-ch:
		t.Fatalf("Drain returned (%v) while a session was still held", err)
	default:
	}
}

// drainReached waits until the drain has reached the runtime (it is on the loop, in order with
// everything the test sent before).
func drainReached(t *testing.T, h *rtharness.H) {
	t.Helper()
	h.WaitFor("the drain to reach the runtime", func() bool { return h.View().Draining })
	h.Sync()
}

func handedOff(h *rtharness.H) bool { return h.Session.HandoffAt(h.SID) != nil }

// listeningSession is a started session that has had one turn: the user spoke, the AI answered,
// the playback is over and turn.completed was sent (turn 1 is COMPLETED, the state LISTENING).
func listeningSession(t *testing.T, opts ...rtharness.Option) *rtharness.H {
	t.Helper()
	h := rtharness.New(t, append([]rtharness.Option{rtharness.WithTimeline(timelineOf(t, "ja_question"))}, opts...)...)
	started(t, h)
	userSays(h, questionJA)
	h.WaitEvents("ai.audio_completed", 1)
	h.Send(h.PlaybackFinishedCmd(lastGeneration(t, h)))
	h.WaitEvents("turn.completed", 1)
	h.Sync()
	require.Equal(t, conversation.StateListening, h.View().TurnState)
	return h
}

// ---- §7.12.1 ----

func TestDrainLISTENING(t *testing.T) {
	h := listeningSession(t)
	rA, _ := h.Runtime()
	fence := h.Session.Fence(h.SID)
	flushed(t, h)
	h.AdvanceClock(3 * time.Second)

	done := drainAsync(t, h.Mgr)
	drained(t, done)
	waitDone(t, rA, "the runtime stopped after the handoff")

	// The client is told to go elsewhere: error{SERVER_DRAINING}, then close 4100.
	code, reason := h.WaitClosed()
	require.Equal(t, 4100, code)
	require.Equal(t, "SERVER_DRAINING", reason)
	p := errorOf(t, h)
	require.Equal(t, "SERVER_DRAINING", p.Error.Code)
	require.True(t, p.Fatal)
	require.True(t, p.Retryable, "the client reconnects at once with a new ticket")
	types := h.Types()
	require.Equal(t, "error", types[len(types)-1], "the error is the last event before the close")

	// The session stays ACTIVE and is not failed: handoff_at is set, nothing else.
	require.Equal(t, session.StatusActive, h.Session.Status(h.SID))
	require.True(t, handedOff(h), "MarkHandoff")
	for _, c := range []string{"BeginEnding", "Complete", "Fail", "AddDisconnected"} {
		require.NotContains(t, h.Calls.Calls(), c, "a drain does not end the session or charge the time away")
	}
	for _, row := range h.Turns.Turns(h.SID) {
		require.NotNil(t, row.Status)
		require.Equal(t, session.TurnCompleted, *row.Status, "no turn is FAILED by a drain that waited for the safepoint")
	}
	for _, a := range h.Turns.Attempts() {
		require.NotEqual(t, "FailInProgress", a.Method)
	}

	// What the next owner reads: rt says disconnected since now, the owner key is released, the
	// events are written (OWNER_CHANGED{DRAIN_RELEASE} last).
	rt := rtOf(t, h)
	require.False(t, rt.Connected)
	require.Equal(t, h.Clock.Now().UnixMilli(), rt.DisconnectedAtMS)
	require.Equal(t, "ACTIVE", rt.Status)
	_, _, held := h.Store.Owner(h.SID)
	require.False(t, held, "the owner key is released: another pod takes over without waiting for the TTL")
	oc := sessionEventsOf(h, "OWNER_CHANGED")
	require.Len(t, oc, 2) // the start, and the release
	require.Equal(t, map[string]any{"reason": "DRAIN_RELEASE", "from_pod": "pod-a", "to_pod": nil, "fence": float64(fence)}, payloadOf(t, oc[1]))

	// The pod itself: draining everywhere, empty.
	info, ok := h.Store.PodInfo("pod-a")
	require.True(t, ok)
	require.True(t, info.Draining, "podinfo.draining=1")
	require.False(t, h.Mgr.Ready())
	require.Equal(t, errs.CodeServerDraining, errs.CodeOf(h.Mgr.Admission()))
	require.Zero(t, h.Mgr.ActiveSessions())
	require.Equal(t, 1.0, h.Metrics.DrainingGauge())
	require.Equal(t, uint64(1), h.Metrics.DrainDurationSamples())
}

// TestDrainWaitsSafepoint: a turn in progress postpones the handoff; DRAIN_SAFEPOINT_TIMEOUT cuts it.
func TestDrainWaitsSafepoint(t *testing.T) {
	h, _ := speakingSession(t)
	rA, _ := h.Runtime()
	require.Equal(t, conversation.StateAISpeaking, h.View().TurnState)

	done := drainAsync(t, h.Mgr)
	drainReached(t, h)
	require.True(t, h.View().Draining)
	require.False(t, h.View().Handoff, "AI_SPEAKING is not a safepoint")
	require.False(t, h.Mgr.Ready())
	stillDraining(t, done)
	require.False(t, isClosed(h.Conn))
	require.False(t, handedOff(h))

	h.AdvanceClock(19 * time.Second)
	h.Tick()
	h.Sync()
	require.False(t, handedOff(h), "19 s: the turn still has a second")
	require.False(t, isClosed(h.Conn))
	require.Equal(t, conversation.StateAISpeaking, h.View().TurnState)

	h.AdvanceClock(time.Second)
	h.Tick()
	code, _ := h.WaitClosed()
	require.Equal(t, 4100, code)
	drained(t, done)
	waitDone(t, rA, "the runtime stopped")

	// The generation was cancelled and the turn FAILED, then the handoff.
	require.Len(t, h.Events("ai.interrupted"), 1)
	row := finalRow(t, h, 1)
	require.Equal(t, session.TurnFailed, *row.Status)
	require.Equal(t, "SERVER_DRAINING", *row.ErrorCode)
	tf := sessionEventsOf(h, "TURN_FAILED")
	require.Len(t, tf, 1)
	require.Equal(t, "DISCONNECT", payloadOf(t, tf[0])["stage"])
	require.Equal(t, "SERVER_DRAINING", payloadOf(t, tf[0])["error_class"])
	require.True(t, handedOff(h))
	require.Equal(t, session.StatusActive, h.Session.Status(h.SID))
	require.NotContains(t, h.Calls.Calls(), "BeginEnding")
	require.Zero(t, h.Session.Disconnected(h.SID))
}

// TestDrainHandsOffWhenTheTurnEnds: a turn that ends before the timeout is not failed: the handoff
// is at the safepoint that follows it (§7.12.1: "tunggu safepoint").
func TestDrainHandsOffWhenTheTurnEnds(t *testing.T) {
	h, gs := speakingSession(t)
	gen := lastGeneration(t, h)
	done := drainAsync(t, h.Mgr)
	drainReached(t, h)
	stillDraining(t, done)
	require.False(t, isClosed(h.Conn))

	gs.limit.Store(1 << 40)
	h.WaitEvents("ai.audio_completed", 1)
	h.Send(h.PlaybackFinishedCmd(gen))
	code, _ := h.WaitClosed()
	require.Equal(t, 4100, code)
	drained(t, done)

	require.Equal(t, "COMPLETED", completed(h)[0].Status, "the answer was heard to the end")
	row := finalRow(t, h, 1)
	require.Equal(t, session.TurnCompleted, *row.Status)
	require.Nil(t, row.ErrorCode)
	require.Empty(t, h.Events("ai.interrupted"))
	require.True(t, handedOff(h))
}

// TestDrainFlushesBeforeHandoff: the persister is flushed (step 1) before handoff_at is set (step
// 3): a turn row still being written holds the handoff back.
func TestDrainFlushesBeforeHandoff(t *testing.T) {
	h := rtharness.New(t, rtharness.WithTimeline(timelineOf(t, "ja_question")))
	started(t, h)
	release := h.Turns.Hold("Finalize")
	t.Cleanup(release)
	userSays(h, questionJA)
	h.WaitEvents("ai.audio_completed", 1)
	h.Send(h.PlaybackFinishedCmd(lastGeneration(t, h)))
	h.WaitEvents("turn.completed", 1)
	h.Sync()
	require.Equal(t, conversation.StateListening, h.View().TurnState)

	done := drainAsync(t, h.Mgr)
	h.WaitFor("the handoff to begin", func() bool { return h.View().Handoff })
	h.Sync()
	require.False(t, handedOff(h), "the final row of the turn is still being written")
	require.NotContains(t, h.Calls.Calls(), "MarkHandoff")
	require.False(t, isClosed(h.Conn))
	stillDraining(t, done)

	release()
	code, _ := h.WaitClosed()
	require.Equal(t, 4100, code)
	drained(t, done)
	require.True(t, handedOff(h))
	row := finalRow(t, h, 1)
	require.Equal(t, session.TurnCompleted, *row.Status, "the row was written before the session was handed off")
}

// TestDrainFlushTimeout: DRAIN_FLUSH_TIMEOUT bounds the flush: a database that does not answer does
// not keep the pod from handing the session off.
func TestDrainFlushTimeout(t *testing.T) {
	h := rtharness.New(t, rtharness.WithTimeline(timelineOf(t, "ja_question")))
	started(t, h)
	release := h.Turns.Hold("Finalize")
	t.Cleanup(release)
	userSays(h, questionJA)
	h.WaitEvents("ai.audio_completed", 1)
	h.Send(h.PlaybackFinishedCmd(lastGeneration(t, h)))
	h.WaitEvents("turn.completed", 1)
	h.Sync()

	done := drainAsync(t, h.Mgr)
	h.WaitFor("the handoff to begin", func() bool { return h.View().Handoff })
	h.AdvanceClock(4900 * time.Millisecond)
	h.Tick()
	h.Sync()
	require.False(t, handedOff(h), "4.9 s: the flush may still finish")

	h.AdvanceClock(100 * time.Millisecond)
	h.Tick()
	code, _ := h.WaitClosed()
	require.Equal(t, 4100, code)
	h.WaitFor("handoff_at", func() bool { return handedOff(h) })
	release() // the write in flight ends; the runtime's exit waits for it
	drained(t, done)
	require.Equal(t, session.StatusActive, h.Session.Status(h.SID))
}

func TestDrainDetachedImmediate(t *testing.T) {
	h := listeningSession(t)
	flushed(t, h)
	h.AdvanceClock(4 * time.Second)
	detach(t, h)
	flushed(t, h)
	lostAt := payloadOf(t, sessionEventsOf(h, "SESSION_DISCONNECTED")[0])["at"].(string)
	from, err := time.Parse(time.RFC3339Nano, lostAt)
	require.NoError(t, err)
	h.AdvanceClock(2 * time.Second)

	// No tick, no clock movement: nobody is connected, so there is nothing to wait for.
	drained(t, drainAsync(t, h.Mgr))
	require.True(t, handedOff(h))
	require.Equal(t, session.StatusActive, h.Session.Status(h.SID))
	rt := rtOf(t, h)
	require.False(t, rt.Connected)
	require.Equal(t, from.UnixMilli(), rt.DisconnectedAtMS, "the time away counts from when the client was lost, not from the drain")
	_, _, held := h.Store.Owner(h.SID)
	require.False(t, held)
	require.Zero(t, callCount(h, "AddDisconnected"), "the next owner deducts the time away (D-055)")
	require.Len(t, sessionEventsOf(h, "OWNER_CHANGED"), 2)
	require.Empty(t, h.Events("error"), "nobody is there to be told")
}

func TestDrainStartingFinishesFirst(t *testing.T) {
	h := rtharness.New(t)
	h.Gates.Config = make(chan struct{})
	h.Connect(h.StartCmd())
	h.WaitFor("the start to wait for the config", func() bool { return h.Blocked() == 1 })

	done := drainAsync(t, h.Mgr)
	h.WaitFor("the drain to reach the runtime", func() bool { return h.View().Draining })
	stillDraining(t, done)
	require.Equal(t, session.StatusStarting, h.Session.Status(h.SID))
	require.False(t, handedOff(h))
	require.False(t, isClosed(h.Conn))
	require.NotContains(t, h.Calls.Calls(), "FailStart", "a drain does not abort a start")

	close(h.Gates.Config)
	code, _ := h.WaitClosed()
	require.Equal(t, 4100, code)
	drained(t, done)
	requireOrder(t, h.Types(), "session.ready", "session.started", "error")
	require.Equal(t, session.StatusActive, h.Session.Status(h.SID), "the start finished, then the session was handed off")
	require.Equal(t, 1, callCount(h, "Activate"))
	require.True(t, handedOff(h))
}

// TestDrainEndingFinishesNormally: a session that is ENDING (here an expiry whose AI is still
// speaking) is not handed off: it finishes and settles on this pod (T22: a hard stop would have left
// it ENDING for the sweeper).
func TestDrainEndingFinishesNormally(t *testing.T) {
	h, gs := speakingSession(t, rtharness.WithDuration(120))
	gen := lastGeneration(t, h)
	advanceTo(h, expiryOf(t, h))
	require.Equal(t, session.StatusEnding, h.View().Status)

	done := drainAsync(t, h.Mgr)
	drainReached(t, h)
	stillDraining(t, done)
	require.False(t, handedOff(h), "an ENDING session is not handed off")
	require.False(t, h.View().Handoff)
	require.False(t, isClosed(h.Conn))
	require.NotContains(t, h.Calls.Calls(), "Complete")

	gs.limit.Store(1 << 40)
	h.WaitEvents("ai.audio_completed", 1)
	h.Send(h.PlaybackFinishedCmd(gen))
	h.WaitEvents("session.completed", 1)
	drained(t, done)

	code, _ := h.WaitClosed()
	require.Equal(t, 1000, code, "closed as a finished session, not 4100")
	require.Equal(t, session.StatusCompleted, h.Session.Status(h.SID))
	require.Equal(t, 1, callCount(h, "Complete"))
	require.NotContains(t, h.Calls.Calls(), "MarkHandoff")
	require.Nil(t, h.Session.HandoffAt(h.SID))
	require.Empty(t, h.Events("error"))
	_, _, held := h.Store.Owner(h.SID)
	require.False(t, held, "the owner key is released by the end, as after any session")
}

// TestDrainWaitsForAPodThatIsStillEnding: the drain returns only when the pod is empty, so the
// caller (which closes the Manager after it) does not cut the tail of an end in progress.
func TestDrainWaitsForAPodThatIsStillEnding(t *testing.T) {
	h, gs := speakingSession(t, rtharness.WithDuration(120))
	advanceTo(h, expiryOf(t, h))
	require.Equal(t, session.StatusEnding, h.View().Status)
	done := drainAsync(t, h.Mgr)
	drainReached(t, h)
	stillDraining(t, done)
	gs.limit.Store(1 << 40)
	// No playback_finished: the grace deadline (expires_at + 10 s) ends the turn.
	h.AdvanceClock(11 * time.Second)
	h.Tick()
	drained(t, done)
	require.Equal(t, session.StatusCompleted, h.Session.Status(h.SID))
	require.NotContains(t, h.Calls.Calls(), "MarkHandoff")
}

// TestResumeAfterDrainOnOtherManager: another pod takes the handed-off session over; handoff_at is
// cleared by the claim, the time away is deducted and no turn is failed (none was running).
func TestResumeAfterDrainOnOtherManager(t *testing.T) {
	h := listeningSession(t)
	lastSeq := h.View().LastDurable
	flushed(t, h)
	drained(t, drainAsync(t, h.Mgr))
	code, _ := h.WaitClosed()
	require.Equal(t, 4100, code)
	require.True(t, handedOff(h))
	fence := h.Session.Fence(h.SID)

	h.AdvanceClock(6 * time.Second)
	b := h.Peer("pod-b")
	b.Connect(b.ResumeCmd(lastSeq))
	snap := b.WaitEvents("session.snapshot", 1)[0].Payload.(protocol.SessionSnapshot)
	b.Sync()
	require.Equal(t, "COMPLETE", snap.Replay, "the drain wrote the last events to the stream")
	require.Nil(t, h.Session.HandoffAt(h.SID), "ClaimRuntime clears handoff_at")
	require.Equal(t, fence+1, h.Session.Fence(h.SID))
	require.Equal(t, session.StatusActive, h.Session.Status(h.SID))
	flushed(t, b)
	require.Equal(t, 6, h.Session.Disconnected(h.SID), "from the drain to the takeover")
	for _, row := range h.Turns.Turns(h.SID) {
		require.Equal(t, session.TurnCompleted, *row.Status)
	}
	require.Empty(t, sessionEventsOf(b, "TURN_FAILED"))
	oc := sessionEventsOf(b, "OWNER_CHANGED")
	require.Len(t, oc, 3) // start, drain release, takeover
	require.Equal(t, "TAKEOVER", payloadOf(t, oc[2])["reason"])
	require.Equal(t, "pod-a", payloadOf(t, oc[2])["from_pod"])
	require.Equal(t, 1, b.Mgr.ActiveSessions())
}

// TestResumeAfterDrainFailsOnlyTheTurnThatRan: the turn the drain cut is FAILED and nothing else is.
func TestResumeAfterDrainFailsOnlyTheTurnThatRan(t *testing.T) {
	h, _ := speakingSession(t)
	lastSeq := lastSeen(h)
	done := drainAsync(t, h.Mgr)
	drainReached(t, h)
	h.AdvanceClock(20 * time.Second)
	h.Tick()
	h.WaitClosed()
	drained(t, done)

	b := h.Peer("pod-b")
	b.Connect(b.ResumeCmd(lastSeq))
	b.WaitEvents("session.snapshot", 1)
	b.Sync()
	flushed(t, b)
	rows := h.Turns.Turns(h.SID)
	require.Len(t, rows, 1)
	require.Equal(t, session.TurnFailed, *rows[0].Status)
	require.Equal(t, "SERVER_DRAINING", *rows[0].ErrorCode, "the takeover found nothing in progress to fail again")
	require.Len(t, sessionEventsOf(b, "TURN_FAILED"), 1, "the drain's TURN_FAILED only")
}

// ---- admission while the pod drains ----

func TestDrainingPodRefusesNewWork(t *testing.T) {
	t.Run("a start", func(t *testing.T) {
		h := rtharness.New(t)
		require.NoError(t, h.Mgr.Drain(t.Context()))
		h.Connect(h.StartCmd())
		p := refusedWith(t, h, 4100, "SERVER_DRAINING")
		require.True(t, p.Retryable)
		require.Equal(t, session.StatusReserved, h.Session.Status(h.SID))
		require.Empty(t, h.Calls.Calls(), "refused before the session is even read")
		require.Equal(t, 1.0, h.Metrics.ConnectionsTotal(conversation.ConnRejectedDraining))
	})

	t.Run("a start that was admitted before the drain began", func(t *testing.T) {
		// The drain begins after Attach's first check and before the slot is reserved: reserve refuses.
		h := rtharness.New(t)
		release := h.Session.Hold("GetForAttach")
		t.Cleanup(release)
		h.Connect(h.StartCmd())
		h.WaitFor("Attach to wait in the session read", func() bool { return h.Session.Blocked() == 1 })
		require.NoError(t, h.Mgr.Drain(t.Context()))
		release()
		refusedWith(t, h, 4100, "SERVER_DRAINING")
		require.Equal(t, session.StatusReserved, h.Session.Status(h.SID))
		require.NotContains(t, h.Calls.Calls(), "ClaimRuntime")
		require.Equal(t, 1.0, h.Metrics.ConnectionsTotal(conversation.ConnRejectedDraining))
		require.Zero(t, h.Mgr.ActiveSessions(), "the refusal holds no slot")
	})

	t.Run("a resume at a pod that has handed the session off", func(t *testing.T) {
		h := rtharness.New(t)
		started(t, h)
		drained(t, drainAsync(t, h.Mgr))
		h.WaitClosed()
		h.Reconnect()
		h.Connect(h.ResumeCmd(0))
		refusedWith(t, h, 4100, "SERVER_DRAINING")
		require.Equal(t, 1, int(h.Session.Fence(h.SID)), "the session is for another pod: this one does not claim it")
	})

	t.Run("a takeover", func(t *testing.T) {
		h := seededTakeover(t, time.Second, nil)
		require.NoError(t, h.Mgr.Drain(t.Context()))
		h.Connect(h.ResumeCmd(30))
		refusedWith(t, h, 4100, "SERVER_DRAINING")
		require.NotContains(t, h.Calls.Calls(), "ClaimRuntime")
	})
}

// TestResumeDuringHandoffIsToldToGoElsewhere: a client that comes back while the handoff runs gets
// 4100 (reconnect at once with a new ticket), not the 4010 of a session that is over.
func TestResumeDuringHandoffIsToldToGoElsewhere(t *testing.T) {
	h := rtharness.New(t, rtharness.WithTimeline(timelineOf(t, "ja_question")))
	started(t, h)
	release := h.Turns.Hold("Finalize")
	t.Cleanup(release)
	userSays(h, questionJA)
	h.WaitEvents("ai.audio_completed", 1)
	h.Send(h.PlaybackFinishedCmd(lastGeneration(t, h)))
	h.WaitEvents("turn.completed", 1)
	h.Sync()
	old := h.Conn

	done := drainAsync(t, h.Mgr)
	h.WaitFor("the handoff to begin", func() bool { return h.View().Handoff })
	h.Reconnect()
	h.Connect(h.ResumeCmd(0))
	refusedWith(t, h, 4100, "SERVER_DRAINING")
	require.Equal(t, session.StatusActive, h.Session.Status(h.SID))
	require.False(t, isClosed(old), "the connection that was being handed off is not cut by the newcomer")

	release()
	code, _ := connClosed(t, old)
	require.Equal(t, 4100, code)
	drained(t, done)
}

// TestDrainCoversAStartInFlight: a start that has reserved its slot but is not registered when the
// drain begins is not forgotten: Drain waits for the pod to be empty, the runtime is told when it
// registers, finishes its start and is handed off.
func TestDrainCoversAStartInFlight(t *testing.T) {
	h := rtharness.New(t)
	release := h.Session.Hold("MarkStarting")
	t.Cleanup(release)
	h.Connect(h.StartCmd())
	h.WaitFor("the start to hold its slot", func() bool {
		return h.Mgr.ActiveSessions() == 1 && callCount(h, "ClaimRuntime") == 1
	})
	_, registered := h.Runtime()
	require.False(t, registered, "no runtime yet: the drain's snapshot is empty")

	done := drainAsync(t, h.Mgr)
	h.WaitFor("the drain to begin", h.Mgr.Draining)
	stillDraining(t, done)
	release()
	code, _ := h.WaitClosed()
	require.Equal(t, 4100, code)
	drained(t, done)
	requireOrder(t, h.Types(), "session.ready", "session.started", "error")
	require.Equal(t, session.StatusActive, h.Session.Status(h.SID))
	require.True(t, handedOff(h))
	require.Zero(t, h.Mgr.ActiveSessions())
}

// TestDrainStopsWithItsContext: a drain that cannot finish returns the error of its context, so the
// caller can fall back to Close (the hard stop).
func TestDrainStopsWithItsContext(t *testing.T) {
	h, _ := speakingSession(t)
	ctx, cancel := context.WithCancel(t.Context())
	errc := make(chan error, 1)
	go func() { errc <- h.Mgr.Drain(ctx) }()
	h.WaitFor("the drain to reach the runtime", func() bool { return h.View().Draining })
	cancel()
	select {
	case err := <-errc:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(5 * time.Second):
		t.Fatal("Drain did not return")
	}
	require.Equal(t, 1, h.Mgr.ActiveSessions(), "the session is still the pod's: Close is what is left")
}
