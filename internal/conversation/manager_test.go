package conversation_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/conversation"
	"github.com/nabePi/kotoki-backend/internal/conversation/protocol"
	"github.com/nabePi/kotoki-backend/internal/platform/errs"
	"github.com/nabePi/kotoki-backend/internal/session"
	"github.com/nabePi/kotoki-backend/internal/testutil/fakes"
	"github.com/nabePi/kotoki-backend/internal/testutil/rtharness"
)

// ---- helpers ----

// refusedWith waits for the connection to be closed and checks the error event and the close code
// of the refusal; it returns the error payload.
func refusedWith(t *testing.T, h *rtharness.H, closeCode int, code string) protocol.ErrorPayload {
	t.Helper()
	got, _ := h.WaitClosed()
	require.Equal(t, closeCode, got)
	p := errorOf(t, h)
	require.Equal(t, code, p.Error.Code)
	return p
}

// connClosed waits for a connection of the harness to be closed and returns its close code.
func connClosed(t *testing.T, c *rtharness.PipeConn) (int, string) {
	t.Helper()
	select {
	case <-c.Closed():
	case <-time.After(5 * time.Second):
		t.Fatal("the connection was not closed")
	}
	code, reason, _ := c.CloseInfo()
	return code, reason
}

// ---- §7.3.2: the status table ----

func TestAttachStatusTable(t *testing.T) {
	t.Run("RESERVED + session.resume: 4009, not retryable", func(t *testing.T) {
		h := rtharness.New(t)
		h.Connect(h.ResumeCmd(0))
		p := refusedWith(t, h, 4009, "SESSION_STATE_CONFLICT")
		require.False(t, p.Retryable, "the client has to session.start")
		require.Equal(t, session.StatusReserved, h.Session.Status(h.SID))
		require.NotContains(t, h.Calls.Calls(), "ClaimRuntime")
	})

	t.Run("STARTING + anything: 4009, retryable", func(t *testing.T) {
		for name, cmd := range map[string]func(*rtharness.H) protocol.ClientEvent{
			"session.start":  (*rtharness.H).StartCmd,
			"session.resume": func(h *rtharness.H) protocol.ClientEvent { return h.ResumeCmd(0) },
		} {
			t.Run(name, func(t *testing.T) {
				h := rtharness.New(t, rtharness.WithStatus(session.StatusStarting))
				h.Connect(cmd(h))
				p := refusedWith(t, h, 4009, "SESSION_STATE_CONFLICT")
				require.True(t, p.Retryable, "the start runs elsewhere: retry after a second")
			})
		}
	})

	t.Run("ACTIVE + session.resume: the resume flow", func(t *testing.T) {
		h := seededTakeover(t, time.Second, nil)
		h.Connect(h.ResumeCmd(30))
		ready := h.WaitEvents("session.ready", 1)[0].Payload.(protocol.SessionReady)
		require.Equal(t, "session.resume", ready.ExpectedCommand)
		require.Equal(t, "ACTIVE", ready.Status)
		snap := h.WaitEvents("session.snapshot", 1)[0].Payload.(protocol.SessionSnapshot)
		require.Equal(t, "COMPLETE", snap.Replay, "last_server_seq 30 is what the previous owner had sent")
	})

	t.Run("ACTIVE + session.start: a resume from 0", func(t *testing.T) {
		h := seededTakeover(t, time.Second, nil)
		h.Connect(h.StartCmd())
		snap := h.WaitEvents("session.snapshot", 1)[0].Payload.(protocol.SessionSnapshot)
		require.Equal(t, "UNAVAILABLE", snap.Replay, "from 0 the client lacks what the owner had sent up to 30, and the stream is gone")
		require.Empty(t, h.Events("session.started"), "a start is not run twice")
		require.Equal(t, 1, h.Mgr.ActiveSessions())
	})

	t.Run("ENDING, COMPLETED, CANCELLED, ABANDONED, FAILED: 4010", func(t *testing.T) {
		for _, st := range []session.Status{session.StatusEnding, session.StatusCompleted, session.StatusCancelled, session.StatusAbandoned, session.StatusFailed} {
			for name, cmd := range map[string]func(*rtharness.H) protocol.ClientEvent{
				"session.start":  (*rtharness.H).StartCmd,
				"session.resume": func(h *rtharness.H) protocol.ClientEvent { return h.ResumeCmd(0) },
			} {
				t.Run(string(st)+" "+name, func(t *testing.T) {
					h := rtharness.New(t, rtharness.WithStatus(st))
					h.Connect(cmd(h))
					refusedWith(t, h, 4010, "SESSION_NOT_RESUMABLE")
					require.NotContains(t, h.Calls.Calls(), "ClaimRuntime")
				})
			}
		}
	})

	t.Run("a session that does not exist: 4004", func(t *testing.T) {
		h := rtharness.New(t)
		h.Ticket.SessionID = uuid.New()
		h.Connect(h.StartCmd())
		refusedWith(t, h, 4004, "SESSION_NOT_FOUND")
	})

	t.Run("another user's session: 4003", func(t *testing.T) {
		h := rtharness.New(t)
		h.Ticket.UserID = uuid.New()
		h.Connect(h.StartCmd())
		refusedWith(t, h, 4003, "FORBIDDEN")
		require.NotContains(t, h.Calls.Calls(), "ClaimRuntime")
	})
}

// ---- §7.12.2: capacity ----

func TestCapacity(t *testing.T) {
	t.Run("ready below 95, starts below 100", func(t *testing.T) {
		h := rtharness.New(t) // the limits of the spec: 95 / 100 / 120
		m := h.Mgr
		require.True(t, m.Ready())
		below := m.HoldSlots(94)
		require.True(t, m.Ready(), "94 sessions: /readyz still 200")
		at95 := m.HoldSlots(1)
		require.False(t, m.Ready(), "/readyz is 503 from 95")
		require.NoError(t, m.Admission(), "connections to a pod that is not ready are still accepted: only the load balancer stops sending")
		require.Equal(t, 95, m.ActiveSessions())
		atMax := m.HoldSlots(5)
		require.Equal(t, 100, m.ActiveSessions())

		// 100 sessions: session.start is refused 4503 and the session stays RESERVED.
		h.Connect(h.StartCmd())
		p := refusedWith(t, h, 4503, "SERVER_AT_CAPACITY")
		require.True(t, p.Retryable)
		require.Equal(t, session.StatusReserved, h.Session.Status(h.SID))
		require.NotContains(t, h.Calls.Calls(), "ClaimRuntime", "a refused start does not take the session")
		require.Equal(t, 1.0, h.Metrics.ConnectionsTotal(conversation.ConnRejectedCapacity))
		require.Equal(t, 100, m.ActiveSessions(), "the refusal holds no slot")

		// 99 sessions: the client's retry is accepted.
		atMax()
		held := m.HoldSlots(4)
		require.Equal(t, 99, m.ActiveSessions())
		h.Reconnect()
		h.Connect(h.StartCmd())
		h.WaitEvents("session.started", 1)
		require.Equal(t, 100, m.ActiveSessions())
		below()
		at95()
		held()
	})

	t.Run("a resume is taken up to 120", func(t *testing.T) {
		h := seededTakeover(t, time.Second, nil)
		hold := h.Mgr.HoldSlots(119) // far above the 100 of the starts
		require.NoError(t, h.Mgr.Admission(), "119 sessions: the upgrade is still allowed")
		h.Connect(h.ResumeCmd(30))
		h.WaitEvents("session.snapshot", 1)
		require.Equal(t, 120, h.Mgr.ActiveSessions())
		require.Equal(t, 2, int(h.Session.Fence(h.SID)))
		require.Equal(t, errs.CodeServerAtCapacity, errs.CodeOf(h.Mgr.Admission()), "120 sessions: the upgrade is refused")
		hold()
	})

	t.Run("a resume at 120 is refused before the session is claimed", func(t *testing.T) {
		h := seededTakeover(t, time.Second, nil)
		hold := h.Mgr.HoldSlots(120)
		err := h.Mgr.Admission()
		require.Error(t, err)
		require.Equal(t, errs.CodeServerAtCapacity, errs.CodeOf(err))
		h.Connect(h.ResumeCmd(30))
		refusedWith(t, h, 4503, "SERVER_AT_CAPACITY")
		require.NotContains(t, h.Calls.Calls(), "ClaimRuntime", "the fence did not move")
		require.Equal(t, 1, int(h.Session.Fence(h.SID)))
		hold()
	})
}

// TestAdmissionBeforeTicket: Admission is what the gateway asks before the upgrade and before the
// ticket is consumed (§7.3.1 step 3, R-22); it is a read of the registry and touches neither Redis
// nor Postgres, so a refusal leaves the ticket alone.
func TestAdmissionBeforeTicket(t *testing.T) {
	h := rtharness.New(t)
	for _, m := range []string{"AcquireOwner", "WriteRT", "ReadRT", "ReadEvents", "ReleaseOwner"} {
		h.Store.FailNext(m, errors.New("must not be called"))
	}
	h.Session.FailNext("GetForAttach", errors.New("must not be called"))
	require.NoError(t, h.Mgr.Admission())
	hold := h.Mgr.HoldSlots(120)
	require.Equal(t, errs.CodeServerAtCapacity, errs.CodeOf(h.Mgr.Admission()))
	hold()
	require.NoError(t, h.Mgr.Drain(t.Context()), "an empty pod drains at once")
	err := h.Mgr.Admission()
	require.Equal(t, errs.CodeServerDraining, errs.CodeOf(err))
	hold = h.Mgr.HoldSlots(120)
	require.Equal(t, errs.CodeServerDraining, errs.CodeOf(h.Mgr.Admission()), "draining wins over capacity: the client is told to go elsewhere")
	hold()

	require.Empty(t, h.Calls.Calls(), "no call of the session facade")
	for _, m := range []string{"AcquireOwner", "WriteRT", "ReadRT", "ReadEvents", "ReleaseOwner"} {
		require.Error(t, h.Store.Take(m), "Admission did not touch the store (%s)", m)
	}
	require.Error(t, h.Session.Take("GetForAttach"))
}

// TestOneConnectionPerSession: a session has one connection; the newest valid one wins and the one
// it replaces is closed 4011 (§7.3.2), on this pod and across pods.
func TestOneConnectionPerSession(t *testing.T) {
	t.Run("on one pod", func(t *testing.T) {
		h := rtharness.New(t)
		started(t, h)
		first := h.Conn
		lastSeq := h.View().LastDurable

		resume(t, h, lastSeq)
		second := h.Conn
		code, reason := connClosed(t, first)
		require.Equal(t, 4011, code)
		require.Equal(t, "SESSION_TAKEN_OVER", reason)
		require.False(t, isClosed(second))

		resume(t, h, h.View().LastDurable)
		third := h.Conn
		code, _ = connClosed(t, second)
		require.Equal(t, 4011, code)
		require.False(t, isClosed(third))
		require.Equal(t, 1, h.Mgr.ActiveSessions())
		require.True(t, h.View().Connected)

		// What the session says now goes to the newest connection only.
		before := len(first.Written()) + len(second.Written())
		h.Send(h.Cmd("ping", protocol.Ping{ClientTS: 1}))
		h.WaitEvents("pong", 1)
		require.Equal(t, before, len(first.Written())+len(second.Written()), "the replaced connections hear nothing more")
	})

	t.Run("across pods", func(t *testing.T) {
		h := rtharness.New(t)
		started(t, h)
		lastSeq := h.View().LastDurable
		catchUp(t, h, true) // pod A's rt is in redis: that is where B learns who to tell
		b := h.Peer("pod-b")
		b.Connect(b.ResumeCmd(lastSeq))
		b.WaitEvents("session.snapshot", 1)
		code, _ := connClosed(t, h.Conn)
		require.Equal(t, 4011, code, "the TAKEOVER stops the owner and its client is told")
		h.WaitFor("pod A to be empty", func() bool { return h.Mgr.ActiveSessions() == 0 })
		require.Equal(t, 1, b.Mgr.ActiveSessions())
	})
}

func isClosed(c *rtharness.PipeConn) bool {
	select {
	case <-c.Closed():
		return true
	default:
		return false
	}
}

// TestActiveSessionsMatchesTheGauge: activeSessions (the capacity figure and the KEDA metric) is
// sessions_active summed over the states, whatever the state of a runtime (§7.12.2).
func TestActiveSessionsMatchesTheGauge(t *testing.T) {
	h := rtharness.New(t)
	same := func(want int) {
		t.Helper()
		require.Equal(t, want, h.Mgr.ActiveSessions())
		require.InDelta(t, float64(want), h.Metrics.SessionsActiveTotal(), 0)
	}
	same(0)
	started(t, h)
	same(1)
	detach(t, h)
	same(1) // a detached session is still the pod's
	r, _ := h.Runtime()
	resume(t, h, 0)
	same(1)
	h.Send(h.EndCmd())
	h.WaitEvents("session.completed", 1)
	waitDone(t, r, "the session ended")
	h.WaitFor("the gauge to follow", func() bool { return h.Metrics.SessionsActiveTotal() == 0 })
	same(0)
}

// ---- §7.9.4: Redis down ----

// TestRedisDownAttach (Review Focus 5, §7.16 #8): with Redis gone a session that is running goes on,
// can be resumed on this pod and ends and settles normally, because everything it needs is in
// memory and Postgres. The refusal of a new handshake (4503) is the gateway's, at its GETDEL of the
// ticket (§7.3.1 step 5b, Task 24); here the Manager's own handshake step that can fail is the
// session read in Postgres, which is answered 4503 as well.
func TestRedisDownAttach(t *testing.T) {
	t.Run("the running session finishes and settles", func(t *testing.T) {
		h := rtharness.New(t, rtharness.WithTimeline(timelineOf(t, "ja_question")))
		started(t, h)
		r, _ := h.Runtime()
		h.Store.FailAlways(errors.New("redis: connection refused"))

		userSays(h, questionJA)
		h.WaitEvents("ai.audio_completed", 1)
		h.Send(h.PlaybackFinishedCmd(lastGeneration(t, h)))
		h.WaitEvents("turn.completed", 1)
		require.Equal(t, "COMPLETED", completed(h)[0].Status)

		// A new connection of the same session is taken with Redis down: the stream is unreadable, which
		// §7.10.4 reads as empty, and the client had everything it was sent.
		detach(t, h)
		snap := resume(t, h, h.View().LastDurable).Payload.(protocol.SessionSnapshot)
		require.Equal(t, "COMPLETE", snap.Replay)
		require.Equal(t, conversation.StateListening, h.View().TurnState)

		h.Send(h.EndCmd())
		done := h.WaitEvents("session.completed", 1)[0].Payload.(protocol.SessionCompleted)
		require.Equal(t, "USER_ENDED", done.EndReason)
		waitDone(t, r, "the session settled")
		require.Equal(t, session.StatusCompleted, h.Session.Status(h.SID))
		require.Equal(t, 1, callCount(h, "Complete"))
		require.Equal(t, session.TurnCompleted, *finalRow(t, h, 1).Status, "the turn reached Postgres")
	})

	t.Run("a handshake whose session read fails: 4503, the running session is untouched", func(t *testing.T) {
		h := rtharness.New(t)
		started(t, h)
		h.Store.FailAlways(errors.New("redis: connection refused"))
		h.Session.FailNext("GetForAttach", errors.New("postgres down"))
		other := h.Peer("pod-b")
		other.Connect(other.StartCmd())
		refusedWith(t, other, 4503, "SERVICE_UNAVAILABLE")
		require.Equal(t, 1.0, other.Metrics.ConnectionsTotal(conversation.ConnRejectedUnavailable))
		require.Equal(t, session.StatusActive, h.Session.Status(h.SID))
		require.True(t, h.View().Connected)
		require.Equal(t, 1, h.Mgr.ActiveSessions())
	})
}

// ---- subscriber and podinfo ----

func TestStartSubscriber(t *testing.T) {
	h := rtharness.New(t)
	store := fakes.NewRuntimeStore()
	deps := h.Deps
	deps.Store = store
	m := conversation.NewManager(deps)
	t.Cleanup(m.Close)
	require.Zero(t, store.Subscribers("pod-a"), "building a Manager subscribes to nothing")

	ctx, cancel := context.WithCancel(t.Context())
	m.StartSubscriber(ctx)
	require.Equal(t, 1, store.Subscribers("pod-a"), "subscribed before StartSubscriber returned: a takeover published now is not lost")
	m.StartSubscriber(ctx)
	require.Equal(t, 1, store.Subscribers("pod-a"), "only the first call counts")
	cancel()
	h.WaitFor("the subscription to end with its context", func() bool { return store.Subscribers("pod-a") == 0 })
}

func TestPodInfoLoop(t *testing.T) {
	h := rtharness.New(t)
	store := fakes.NewRuntimeStore()
	deps := h.Deps
	deps.Store, deps.Build = store, "v1.2.3"
	m := conversation.NewManager(deps)
	t.Cleanup(m.Close)

	ctx, cancel := context.WithCancel(t.Context())
	exited := make(chan struct{})
	go func() { m.PodInfoLoop(ctx); close(exited) }()
	every5s := func(d time.Duration) bool { return d == 5*time.Second }

	h.WaitFor("the first podinfo", func() bool { _, ok := store.PodInfo("pod-a"); return ok })
	got, _ := store.PodInfo("pod-a")
	require.Equal(t, 0, got.ActiveSessions)
	require.False(t, got.Draining)
	require.Equal(t, "v1.2.3", got.Build)
	require.True(t, got.StartedAt.Equal(rtharness.T0))

	hold := m.HoldSlots(3)
	require.Equal(t, 1, h.Tickers.Fire(h.Clock.Now(), every5s), "one ticker of 5 s")
	h.WaitFor("active_sessions to follow", func() bool { p, _ := store.PodInfo("pod-a"); return p.ActiveSessions == 3 })

	store.FailNext("WritePodInfo", errors.New("redis down")) // a failed write is tried again at the next interval
	hold()
	require.Equal(t, 1, h.Tickers.Fire(h.Clock.Now(), every5s))
	require.Equal(t, 1, h.Tickers.Fire(h.Clock.Now(), every5s))
	h.WaitFor("the write after the failure", func() bool { p, _ := store.PodInfo("pod-a"); return p.ActiveSessions == 0 })

	cancel()
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		t.Fatal("PodInfoLoop did not stop with its context")
	}
}
