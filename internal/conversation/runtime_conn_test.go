package conversation_test

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/conversation"
	"github.com/nabePi/kotoki-backend/internal/conversation/protocol"
	"github.com/nabePi/kotoki-backend/internal/conversation/rtstate"
	"github.com/nabePi/kotoki-backend/internal/learner"
	"github.com/nabePi/kotoki-backend/internal/memory"
	"github.com/nabePi/kotoki-backend/internal/provider"
	"github.com/nabePi/kotoki-backend/internal/session"
	"github.com/nabePi/kotoki-backend/internal/testutil/fakes"
	"github.com/nabePi/kotoki-backend/internal/testutil/rtharness"
)

// ---- helpers ----

// speakingSession starts a session and brings it to AI_SPEAKING with the pacer held: the first
// sentence of the reply is written, the second is parsed and half queued (the same state the
// barge-in tests use). The returned gate lets a test free the pacer again.
func speakingSession(t *testing.T, opts ...rtharness.Option) (*rtharness.H, *gatedSleep) {
	t.Helper()
	gs := &gatedSleep{}
	opts = append([]rtharness.Option{rtharness.WithScript(longScript), rtharness.WithSleep(gs.sleep)}, opts...)
	h := rtharness.New(t, opts...)
	gs.clk = h.Clock
	gs.limit.Store(40)
	started(t, h)
	userSays(h, questionJA)
	h.WaitEvents("ai.audio_started", 1)
	h.WaitFor("the pacer is held", gs.held.Load)
	h.Sync()
	return h, gs
}

// lastSeen is the largest server_seq the client of the current connection has received.
func lastSeen(h *rtharness.H) int64 {
	var last int64
	for _, ev := range h.Events() {
		last = max(last, ev.ServerSeq)
	}
	return last
}

// eventsOn returns the server events written to a connection, in order, of the given types.
func eventsOn(t *testing.T, c *rtharness.PipeConn, types ...string) []protocol.ServerEvent {
	t.Helper()
	var out []protocol.ServerEvent
	for _, w := range c.Written() {
		if w.Kind != conversation.MessageText {
			continue
		}
		ev, err := protocol.DecodeServerEvent(w.Data)
		require.NoError(t, err)
		if len(types) == 0 {
			out = append(out, ev)
			continue
		}
		for _, ty := range types {
			if ev.Type == ty {
				out = append(out, ev)
			}
		}
	}
	return out
}

func rtOf(t *testing.T, h *rtharness.H) rtstate.RT {
	t.Helper()
	rt, ok, err := h.Store.ReadRT(t.Context(), h.SID)
	require.NoError(t, err)
	require.True(t, ok, "no rt hash in redis")
	return rt
}

// streamEvents reads the replay stream.
func streamEvents(t *testing.T, h *rtharness.H) []protocol.ServerEvent {
	t.Helper()
	entries, err := h.Store.ReadEvents(t.Context(), h.SID)
	require.NoError(t, err)
	var out []protocol.ServerEvent
	for _, e := range entries {
		ev, err := protocol.DecodeServerEvent(e.Env)
		require.NoError(t, err)
		out = append(out, ev)
	}
	return out
}

// catchUp lets the redisSync of h write what it holds: the rt hash has the current numbering and
// connection state, the replay stream the last durable event, and (when a turn runs) the turn hash.
func catchUp(t *testing.T, h *rtharness.H, connected bool) {
	t.Helper()
	h.AdvanceClock(300 * time.Millisecond) // past the coalesce windows of the rt (100 ms) and turn (250 ms) hashes
	h.WaitFor("the redis state to catch up", func() bool {
		h.Tick()
		v := h.View()
		rt, ok, _ := h.Store.ReadRT(t.Context(), h.SID)
		if !ok || rt.PodID == "" || rt.Connected != connected || rt.LastServerSeq < v.ServerSeq || rt.LastDurableSeq < v.LastDurable {
			return false
		}
		entries, _ := h.Store.ReadEvents(t.Context(), h.SID)
		return len(entries) > 0 && entries[len(entries)-1].Seq >= v.LastDurable
	})
}

// flushed waits until the runtime's persister has written everything it was given.
func flushed(t *testing.T, h *rtharness.H) {
	t.Helper()
	r, ok := h.Runtime()
	require.True(t, ok)
	require.NoError(t, r.FlushPersister(t.Context()))
}

// detach drops the client's connection and waits until the runtime has noticed.
func detach(t *testing.T, h *rtharness.H) {
	t.Helper()
	h.Conn.EOF()
	h.WaitFor("the runtime to notice the disconnect", func() bool { return !h.View().Connected })
	h.Sync()
}

// resume reconnects the client and sends session.resume{lastSeq}; it returns the snapshot.
func resume(t *testing.T, h *rtharness.H, lastSeq int64) protocol.ServerEvent {
	t.Helper()
	h.Reconnect()
	h.Connect(h.ResumeCmd(lastSeq))
	return h.WaitEvents("session.snapshot", 1)[0]
}

// sttClosed waits until the latest STT stream is closed (the runtime closes it off the loop).
func sttClosed(t *testing.T, h *rtharness.H) {
	t.Helper()
	h.WaitFor("the STT stream to close", func() bool {
		return errors.Is(h.Router.STT().SendAudio(make([]byte, 640)), provider.ErrSessionClosed)
	})
}

func sessionEventsOf(h *rtharness.H, typ string) []fakes.WrittenEvent {
	var out []fakes.WrittenEvent
	for _, ev := range h.Written.Events() {
		if ev.Type == typ {
			out = append(out, ev)
		}
	}
	return out
}

func payloadOf(t *testing.T, ev fakes.WrittenEvent) map[string]any {
	t.Helper()
	var m map[string]any
	require.NoError(t, json.Unmarshal(ev.Payload, &m))
	return m
}

// waitDone waits for the runtime to stop.
func waitDone(t *testing.T, r *conversation.Runtime, what string) {
	t.Helper()
	select {
	case <-r.Done():
	case <-time.After(5 * time.Second):
		t.Fatalf("the runtime did not stop: %s", what)
	}
}

// seedTurn writes a final turn row as the previous owner would have left it (fence 1).
func seedTurn(t *testing.T, h *rtharness.H, seq, gen int, status session.TurnStatus, user, assistant, spoken string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	now := rtharness.T0
	require.NoError(t, h.Turns.Insert(t.Context(), 1, conversation.TurnStart{ID: id, SessionID: h.SID, Sequence: seq, Generation: gen, CreatedAt: now}))
	if user != "" {
		require.NoError(t, h.Turns.UpdateUser(t.Context(), 1, id, conversation.TurnUser{CreatedAt: now, Text: user}))
	}
	fin := conversation.TurnFinal{CreatedAt: now, Status: status, CompletedAt: now}
	if assistant != "" {
		fin.AssistantText = &assistant
	}
	if spoken != "" {
		fin.AssistantSpokenText = &spoken
	}
	require.NoError(t, h.Turns.Finalize(t.Context(), 1, id, fin))
	return id
}

// seededTakeover is a harness whose session is ACTIVE and owned by another pod: its rt hash is in
// redis (disconnected from `away` before the fake clock) and its fence is 1. A resume to this
// harness is a takeover.
func seededTakeover(t *testing.T, away time.Duration, mutate func(*rtstate.RT), opts ...rtharness.Option) *rtharness.H {
	t.Helper()
	h := rtharness.New(t, append([]rtharness.Option{rtharness.WithStatus(session.StatusActive)}, opts...)...)
	rt := rtstate.RT{
		Status: "ACTIVE", TurnState: "LISTENING", AcceptingTurns: true, Generation: 4, NextTurnSeq: 2, LastServerSeq: 30, LastDurableSeq: 28,
		ExpiresAtMS: rtharness.T0.Add(600 * time.Second).UnixMilli(), PodID: "pod-x", Fence: 1,
		DisconnectedAtMS: h.Clock.Now().Add(-away).UnixMilli(),
	}
	if mutate != nil {
		mutate(&rt)
	}
	require.NoError(t, h.Store.WriteRT(t.Context(), h.SID, rt, time.Hour))
	return h
}

// ---- disconnect (§7.10.1) ----

func TestDisconnectMidTurn(t *testing.T) {
	h, _ := speakingSession(t)
	old := lastGeneration(t, h)
	catchUp(t, h, true)
	h.Conn.EOF()
	h.WaitFor("the runtime to notice the disconnect", func() bool { return !h.View().Connected })
	h.Sync()
	flushed(t, h)

	// The session is still ACTIVE and listening, the generation of the interrupted reply is gone.
	v := h.View()
	require.Equal(t, session.StatusActive, v.Status)
	require.Equal(t, conversation.StateListening, v.TurnState)
	require.True(t, v.Accepting)
	require.Equal(t, uuid.Nil, v.CurrentTurn, "the turn in progress ended")
	require.Equal(t, old+1, v.Generation, "the generation moved on when it was cancelled")

	// The turn is FAILED with what it had so far; a disconnect is not a provider failure.
	row := finalRow(t, h, 1)
	require.Equal(t, session.TurnFailed, *row.Status)
	require.Equal(t, "CLIENT_DISCONNECTED", *row.ErrorCode)
	require.NotNil(t, row.AssistantText)
	require.Equal(t, "こんにちは。", *row.AssistantSpokenText, "the one sentence the user heard in full")
	require.Equal(t, 0, v.FailedTurns, "it is the connection that failed, not a provider")
	tf := sessionEventsOf(h, "TURN_FAILED")
	require.Len(t, tf, 1)
	require.Equal(t, row.ID, *tf[0].TurnID)
	require.Equal(t, map[string]any{"turn_seq": float64(1), "stage": "DISCONNECT", "error_class": "CLIENT_DISCONNECTED"}, payloadOf(t, tf[0]))
	dc := sessionEventsOf(h, "SESSION_DISCONNECTED")
	require.Len(t, dc, 1)
	require.NotEmpty(t, payloadOf(t, dc[0])["at"])

	// ai.interrupted and turn.completed went to the replay stream only: the dead connection got none.
	require.Empty(t, h.Events("ai.interrupted"))
	require.Empty(t, h.Events("turn.completed"))
	catchUp(t, h, false)
	var interrupted, completedEv *protocol.ServerEvent
	for _, ev := range streamEvents(t, h) {
		switch ev.Type {
		case "ai.interrupted":
			interrupted = &ev
		case "turn.completed":
			completedEv = &ev
		}
	}
	require.NotNil(t, interrupted, "ai.interrupted is in the replay stream")
	ip := interrupted.Payload.(protocol.AIInterrupted)
	require.Equal(t, "DISCONNECTED", ip.Reason)
	require.Equal(t, old+1, ip.ActiveGeneration)
	require.Equal(t, 1, ip.SpokenSentenceCount)
	require.NotNil(t, completedEv, "turn.completed{FAILED} is in the replay stream")
	require.Equal(t, "FAILED", completedEv.Payload.(protocol.TurnCompleted).Status)

	// The STT stream is closed: nothing is sent while the client is away.
	sttClosed(t, h)

	// rt says so, with the time the client was last heard.
	rt := rtOf(t, h)
	require.False(t, rt.Connected)
	require.Positive(t, rt.DisconnectedAtMS)
	require.Equal(t, "LISTENING", rt.TurnState)

	// balance.tick stops while detached (the schedule is kept, nothing is issued); the timers go on.
	seq := h.View().ServerSeq
	h.AdvanceClock(10 * time.Second)
	h.Tick()
	h.Sync()
	require.Equal(t, seq, h.View().ServerSeq, "no balance.tick without a client")
	require.Equal(t, session.StatusActive, h.View().Status)
}

func TestDisconnectDuringUserSpeech(t *testing.T) {
	h := rtharness.New(t)
	started(t, h)
	h.SpeakSlice("ja_question", 0, 700)
	h.Router.STT().EmitPartial("趣味は")
	h.WaitEvents("stt.partial", 1)
	h.Sync()
	require.Equal(t, conversation.StateUserSpeaking, h.View().TurnState)
	detach(t, h)
	flushed(t, h)

	row := finalRow(t, h, 1)
	require.Equal(t, session.TurnFailed, *row.Status)
	require.Equal(t, "趣味は", *row.UserText, "the words so far are the user's side of the turn")
	require.Nil(t, row.AssistantText)
	tf := sessionEventsOf(h, "TURN_FAILED")
	require.Len(t, tf, 1)
	require.Equal(t, "DISCONNECT", payloadOf(t, tf[0])["stage"])
	require.Equal(t, conversation.StateListening, h.View().TurnState)

	// The resumed client sees the turn in recent_turns, and the next one is turn 2.
	snap := resume(t, h, lastSeen(h)).Payload.(protocol.SessionSnapshot)
	require.Equal(t, 2, snap.NextTurnSeq)
	require.Len(t, snap.RecentTurns, 1)
	require.Equal(t, "FAILED", snap.RecentTurns[0].Status)
	require.Equal(t, "趣味は", *snap.RecentTurns[0].UserText)

	// The detector and the STT text start clean: the half utterance of before does not leak into
	// the next turn, which is heard and answered like any other.
	h.Router.STT().SetFinalizeText(questionJA)
	h.Speak("ja_greeting_short")
	h.Silence(1200)
	fin := h.WaitEvents("stt.final", 1)[0]
	require.Equal(t, 2, *fin.TurnSeq)
	require.Equal(t, questionJA, fin.Payload.(protocol.STTFinal).Text)
}

func TestDisconnectWhileListeningCreatesNoTurn(t *testing.T) {
	h := rtharness.New(t)
	started(t, h)
	detach(t, h)
	flushed(t, h)
	require.Empty(t, h.Turns.Turns(h.SID))
	require.Empty(t, sessionEventsOf(h, "TURN_FAILED"))
	require.Len(t, sessionEventsOf(h, "SESSION_DISCONNECTED"), 1)
	require.Equal(t, conversation.StateListening, h.View().TurnState)
}

func TestTimersRunWhileDetached(t *testing.T) {
	h := rtharness.New(t, rtharness.WithDuration(80))
	started(t, h)
	lastSeq := lastSeen(h)
	detach(t, h)
	h.AdvanceClock(21 * time.Second) // T-60 s of an 80 s session, inside the 30 s window
	h.Tick()
	h.Sync()
	require.Equal(t, session.StatusActive, h.View().Status)

	// The warning was issued while nobody was there: the resumed client gets it from the replay.
	snap := resume(t, h, lastSeq)
	replayed := h.Events("session.ending")
	require.Len(t, replayed, 1)
	require.True(t, replayed[0].Replay)
	require.Equal(t, protocol.SessionEnding{Stage: "WARNING", Reason: "DURATION_ELAPSED", RemainingSeconds: 60}, replayed[0].Payload)
	require.Equal(t, "COMPLETE", snap.Payload.(protocol.SessionSnapshot).Replay)
	require.Equal(t, 59, snap.Payload.(protocol.SessionSnapshot).Session.RemainingSeconds)
}

// ---- resume on the same pod (§7.10.2) ----

func TestResumeSamePod(t *testing.T) {
	h, gs := speakingSession(t)
	old := lastGeneration(t, h)
	before := h.Events()
	lastSeq := lastSeen(h)
	seenIDs := map[uuid.UUID]bool{}
	for _, ev := range before {
		seenIDs[ev.EventID] = true
	}
	detach(t, h)
	catchUp(t, h, false)
	from := time.UnixMilli(rtOf(t, h).DisconnectedAtMS)
	h.Clock.Set(from.Add(7*time.Second + 500*time.Millisecond)) // 7.5 s away: floor is 7, ceil would be 8

	h.Reconnect()
	h.Connect(h.ResumeCmd(lastSeq))
	snapEv := h.WaitEvents("session.snapshot", 1)[0]
	h.Sync()

	// What the client missed arrives with replay=true and the seq it had, then the snapshot; the
	// session is not started again and no audio is replayed.
	var types []string
	for _, ev := range h.Events() {
		types = append(types, ev.Type)
	}
	requireOrder(t, types, "session.ready", "ai.interrupted", "turn.completed", "session.snapshot")
	require.NotContains(t, types, "session.started")
	require.Empty(t, h.Frames(), "audio is never replayed")
	var replayed []protocol.ServerEvent
	for _, ev := range h.Events() {
		if ev.Replay {
			replayed = append(replayed, ev)
		}
	}
	require.Len(t, replayed, 2)
	for i, ev := range replayed {
		require.Greater(t, ev.ServerSeq, lastSeq, "only what the client did not have")
		require.False(t, seenIDs[ev.EventID], "no duplicate of an event the client processed")
		if i > 0 {
			require.Greater(t, ev.ServerSeq, replayed[i-1].ServerSeq)
		}
	}
	require.Equal(t, "DISCONNECTED", replayed[0].Payload.(protocol.AIInterrupted).Reason)
	require.Equal(t, "FAILED", replayed[1].Payload.(protocol.TurnCompleted).Status)

	snap := snapEv.Payload.(protocol.SessionSnapshot)
	require.Equal(t, "COMPLETE", snap.Replay)
	require.Equal(t, 2, snap.ReplayedCount)
	require.Equal(t, snapEv.ServerSeq, snap.ServerSeqBaseline, "the snapshot's own seq is the client's new baseline")
	require.Greater(t, snapEv.ServerSeq, replayed[1].ServerSeq)
	require.Equal(t, old+1, snap.ActiveGeneration)
	require.Equal(t, old+1, *snapEv.Generation)
	require.Equal(t, "LISTENING", snap.TurnState)
	require.Equal(t, 2, snap.NextTurnSeq)
	require.Equal(t, "ACTIVE", snap.Session.Status)
	require.Len(t, snap.RecentTurns, 1, "the turn that failed is a final turn of the snapshot")
	require.Equal(t, "FAILED", snap.RecentTurns[0].Status)

	// The time away: whole seconds only; the STT stream is a new one; the event is recorded.
	flushed(t, h)
	require.Equal(t, 7, h.Session.Disconnected(h.SID))
	require.Equal(t, 2, h.Router.STTOpens())
	rc := sessionEventsOf(h, "SESSION_RECONNECTED")
	require.Len(t, rc, 1)
	require.Equal(t, map[string]any{"window_seconds": float64(7), "replay": "COMPLETE", "same_pod": true}, payloadOf(t, rc[0]))
	v := h.View()
	require.True(t, v.Connected)
	require.Equal(t, session.StatusActive, v.Status)
	require.Equal(t, 1, h.Mgr.Active())

	// The session works after the resume: a new user turn is heard and answered.
	gs.limit.Store(1 << 40)
	h.Router.STT().SetFinalizeText(questionJA)
	h.Speak("ja_greeting_short")
	h.Silence(1200)
	fin := h.WaitEvents("stt.final", 1)[0]
	require.Equal(t, questionJA, fin.Payload.(protocol.STTFinal).Text)
	require.Equal(t, 2, *fin.TurnSeq)
	gens := h.WaitEvents("ai.generation_started", 1)
	require.Equal(t, old+2, *gens[0].Generation)
}

func TestDisconnectedSecondsAreWholeSeconds(t *testing.T) {
	for _, tc := range []struct {
		away time.Duration
		want int
	}{
		{999 * time.Millisecond, 0},
		{1 * time.Second, 1},
		{12*time.Second + 990*time.Millisecond, 12},
	} {
		t.Run(tc.away.String(), func(t *testing.T) {
			h := rtharness.New(t)
			started(t, h)
			lastSeq := lastSeen(h)
			detach(t, h)
			catchUp(t, h, false)
			from := time.UnixMilli(rtOf(t, h).DisconnectedAtMS)
			h.Clock.Set(from.Add(tc.away))
			resume(t, h, lastSeq)
			flushed(t, h)
			require.Equal(t, tc.want, h.Session.Disconnected(h.SID))
			if tc.want == 0 {
				require.NotContains(t, h.Calls.Calls(), "AddDisconnected")
			}
		})
	}
}

func TestReconnectTimeout30s(t *testing.T) {
	h := rtharness.New(t)
	started(t, h)
	r, _ := h.Runtime()
	detach(t, h)
	h.AdvanceClock(29 * time.Second)
	h.Tick()
	h.Sync()
	require.Equal(t, session.StatusActive, h.View().Status, "the window is 30 s")

	h.AdvanceClock(time.Second)
	h.Tick()
	waitDone(t, r, "the reconnect window passed")

	require.Equal(t, 30, h.Session.Disconnected(h.SID), "the whole time away is recorded before the session ends")
	requireOrder(t, h.Calls.Calls(), "AddDisconnected", "BeginEnding", "Complete")
	require.Equal(t, session.StatusCompleted, h.Session.Status(h.SID))
	// There is nobody to tell; the events are in the replay stream for a client that comes back.
	require.Empty(t, h.Events("session.ending"))
	var ending *protocol.SessionEnding
	for _, ev := range streamEvents(t, h) {
		if p, ok := ev.Payload.(protocol.SessionEnding); ok {
			ending = &p
		}
	}
	require.NotNil(t, ending)
	require.Equal(t, "ENDING", ending.Stage)
	require.Equal(t, "RECONNECT_TIMEOUT", ending.Reason)
	require.Equal(t, 1, int(h.Metrics.ConnectionsTotal("accepted")))

	// A client that comes back now is told the session is over.
	h.Reconnect()
	h.Connect(h.ResumeCmd(0))
	require.Error(t, h.AttachResult())
	code, _ := h.WaitClosed()
	require.Equal(t, 4010, code)
}

func TestExpiryWhileDetachedCountsAwayTime(t *testing.T) {
	h := rtharness.New(t, rtharness.WithDuration(20))
	started(t, h)
	r, _ := h.Runtime()
	detach(t, h)
	h.AdvanceClock(20 * time.Second)
	h.Tick()
	waitDone(t, r, "the session expired while the client was away")

	requireOrder(t, h.Calls.Calls(), "AddDisconnected", "BeginEnding", "Complete")
	require.Equal(t, 20, h.Session.Disconnected(h.SID))
	charged := -1
	for _, ev := range streamEvents(t, h) {
		if p, ok := ev.Payload.(protocol.SessionCompleted); ok {
			charged = p.ChargedSeconds
			require.Equal(t, "DURATION_ELAPSED", p.EndReason)
			require.Equal(t, 20, p.DisconnectedSeconds)
		}
	}
	require.Equal(t, 0, charged, "twenty seconds of a twenty second session were spent away: nothing is charged")
}

func TestOldConnClosed4011(t *testing.T) {
	h := rtharness.New(t)
	started(t, h)
	old := h.Conn
	lastSeq := lastSeen(h)
	h.AdvanceClock(5*time.Second + 300*time.Millisecond) // the old connection says nothing meanwhile

	snap := resume(t, h, lastSeq)
	require.Equal(t, "COMPLETE", snap.Payload.(protocol.SessionSnapshot).Replay)
	select {
	case <-old.Closed():
	case <-time.After(5 * time.Second):
		t.Fatal("the old connection was not closed")
	}
	code, reason, _ := old.CloseInfo()
	require.Equal(t, 4011, code)
	require.Equal(t, "SESSION_TAKEN_OVER", reason)
	errEv := eventsOn(t, old, "error")
	require.NotEmpty(t, errEv)
	require.Equal(t, "SESSION_TAKEN_OVER", errEv[len(errEv)-1].Payload.(protocol.ErrorPayload).Error.Code)

	// The old connection counts as disconnected from its last activity.
	flushed(t, h)
	require.Equal(t, 5, h.Session.Disconnected(h.SID))
	require.Len(t, sessionEventsOf(h, "SESSION_DISCONNECTED"), 1)
	require.True(t, h.View().Connected)
	require.Equal(t, 1, h.Mgr.Active())
}

func TestResumeRejectedWhenEnding(t *testing.T) {
	h := rtharness.New(t)
	started(t, h)
	r, _ := h.Runtime()
	lastSeq := lastSeen(h)
	detach(t, h)
	require.NoError(t, r.SetEnding(t.Context())) // the end has begun (Task 22 reaches it during the expiry grace)

	h.Reconnect()
	h.Connect(h.ResumeCmd(lastSeq))
	require.Error(t, h.AttachResult())
	code, _ := h.WaitClosed()
	require.Equal(t, 4010, code)
	errEv := h.Events("error")
	require.Len(t, errEv, 1)
	require.Equal(t, "SESSION_NOT_RESUMABLE", errEv[0].Payload.(protocol.ErrorPayload).Error.Code)
	require.Equal(t, 1, h.Router.STTOpens(), "no stream was opened for a refused resume")
}

func TestResumeSTTFailureEndsSession(t *testing.T) {
	h := rtharness.New(t)
	started(t, h)
	r, _ := h.Runtime()
	lastSeq := lastSeen(h)
	detach(t, h)
	h.Router.SetOpenError(provider.NewError(provider.FakePrimary, provider.CapSTT, provider.KindUnavailable, "no stream"))

	h.Reconnect()
	h.Connect(h.ResumeCmd(lastSeq))
	waitDone(t, r, "the STT stream of a resume did not open")
	failed := h.Events("session.failed")
	require.Len(t, failed, 1)
	require.Equal(t, "PROVIDER_FAILURE", failed[0].Payload.(protocol.SessionFailed).EndReason)
	code, _ := h.WaitClosed()
	require.Equal(t, 4500, code)
	require.Equal(t, session.StatusFailed, h.Session.Status(h.SID))
	require.Empty(t, h.Events("session.snapshot"))
}

func TestReplayUnavailableCases(t *testing.T) {
	t.Run("the client claims a seq the server never issued", func(t *testing.T) {
		h := rtharness.New(t)
		started(t, h)
		detach(t, h)
		snap := resume(t, h, 99999).Payload.(protocol.SessionSnapshot)
		require.Equal(t, "UNAVAILABLE", snap.Replay)
		require.Zero(t, snap.ReplayedCount)
		for _, ev := range h.Events() {
			require.False(t, ev.Replay)
		}
	})
	t.Run("the replay stream cannot be read and the client is behind", func(t *testing.T) {
		h := rtharness.New(t)
		started(t, h)
		detach(t, h)
		h.Store.FailNext("ReadEvents", errors.New("redis down"))
		snap := resume(t, h, 1).Payload.(protocol.SessionSnapshot)
		require.Equal(t, "UNAVAILABLE", snap.Replay)
	})
	t.Run("the replay stream cannot be read but the client has everything", func(t *testing.T) {
		h := rtharness.New(t)
		started(t, h)
		last := lastSeen(h)
		detach(t, h)
		h.Store.FailNext("ReadEvents", errors.New("redis down"))
		snap := resume(t, h, last).Payload.(protocol.SessionSnapshot)
		require.Equal(t, "COMPLETE", snap.Replay, "known to be nothing missed: the numbering is in memory")
	})
	t.Run("a gap longer than the queue can take is answered by the snapshot", func(t *testing.T) {
		h := rtharness.New(t)
		started(t, h)
		detach(t, h)
		flushed(t, h)
		for i := range 200 { // a stream that could be replayed whole, if it fitted
			require.NoError(t, h.Store.AppendEvent(t.Context(), h.SID, rtstate.EventEntry{Seq: int64(1000 + i), PrevSeq: int64(999 + i), Type: "turn.completed"}))
		}
		snap := resume(t, h, 0).Payload.(protocol.SessionSnapshot)
		require.Equal(t, "UNAVAILABLE", snap.Replay)
		require.Zero(t, snap.ReplayedCount)
	})
}

func TestSTTClosedUnrequestedFailsSession(t *testing.T) {
	h := rtharness.New(t)
	started(t, h)
	r, _ := h.Runtime()
	require.NoError(t, h.Router.STT().Close()) // the vendor ended the stream; nobody asked for it
	waitDone(t, r, "the STT stream closed under the session")
	failed := h.WaitEvents("session.failed", 1)
	require.Equal(t, "PROVIDER_FAILURE", failed[0].Payload.(protocol.SessionFailed).EndReason)
	code, _ := h.WaitClosed()
	require.Equal(t, 4500, code)
	require.Equal(t, session.StatusFailed, h.Session.Status(h.SID))
	require.Empty(t, sessionEventsOf(h, "PROVIDER_FALLBACK"))
}

func TestSTTClosedByDisconnectIsNotAFailure(t *testing.T) {
	h := rtharness.New(t)
	started(t, h)
	detach(t, h) // the runtime itself closes the stream: its CLOSED must not end the session
	h.AdvanceClock(time.Second)
	h.Tick()
	h.Sync()
	require.Equal(t, session.StatusActive, h.View().Status)
	require.Empty(t, h.Events("session.failed"))
}

func TestResumePlaysThePostponedOpening(t *testing.T) {
	h := rtharness.New(t, rtharness.WithStartMode(session.StartAIFirst))
	h.Gates.Config = make(chan struct{})
	h.Connect(h.StartCmd())
	h.WaitFor("the start to wait for the config", func() bool { return h.Blocked() == 1 })
	h.Conn.EOF() // the client leaves while the session is starting
	h.WaitFor("the runtime to notice", func() bool { return !h.View().Connected })
	close(h.Gates.Config)
	h.WaitFor("the session to become ACTIVE", func() bool { return h.View().Status == session.StatusActive })
	h.Sync()
	require.Empty(t, h.Turns.Turns(h.SID), "nobody to speak to: the opening waits")
	require.Equal(t, 1, h.Router.STTOpens())
	sttClosed(t, h) // a session with no client holds no STT stream

	h.Reconnect()
	h.Connect(h.ResumeCmd(0))
	h.WaitEvents("session.snapshot", 1)
	started := h.WaitEvents("ai.generation_started", 1)
	require.Equal(t, "OPENING", started[0].Payload.(protocol.AIGenerationStarted).Kind)
	require.Equal(t, 2, h.Router.STTOpens())
}

// ---- takeover (§7.10.3) ----

// TestOldOwnerWritesNothingAfterTakeover: the database is slow, the persister of the old owner holds
// a queue behind the write in flight, and the new owner claims the session. The old owner must stop
// on the TAKEOVER message, at once, and of everything it had queued only the write already in
// flight may still reach the database (where the fence refuses it).
func TestOldOwnerWritesNothingAfterTakeover(t *testing.T) {
	h := rtharness.New(t)
	started(t, h)
	r, _ := h.Runtime()
	release := h.Turns.Hold("Insert")
	t.Cleanup(release)
	userSays(h, questionJA) // the turn's rows queue behind the held INSERT
	repliedTo(t, h, 1)
	h.WaitFor("a queue behind the write in flight", func() bool { return r.PersisterPending() >= 2 })

	// Pod B claims the session (the fence rises, the owner key is B's) and tells pod A.
	newFence, _, err := h.Session.ClaimRuntime(t.Context(), h.SID, []session.Status{session.StatusActive})
	require.NoError(t, err)
	ok, err := h.Store.AcquireOwner(t.Context(), h.SID, "pod-b", newFence, 15*time.Second)
	require.NoError(t, err)
	require.True(t, ok)
	// What pod B has written to redis since: pod A has state of its own that it never wrote (nothing
	// ticked), and must not write it over B's now.
	require.NoError(t, h.Store.WriteRT(t.Context(), h.SID, rtstate.RT{Status: "ACTIVE", PodID: "pod-b", Fence: newFence, Generation: 99}, time.Hour))
	turnWrites, eventWrites := len(h.Turns.Attempts()), len(h.Written.Attempts())
	require.NoError(t, h.Store.PublishTakeover(t.Context(), "pod-a", rtstate.Takeover{SessionID: h.SID, Fence: newFence}))
	h.WaitFor("the old owner to stop", func() bool { _, alive := h.Mgr.Runtime(h.SID); return !alive || r.PersisterPending() == 0 })
	release()

	waitDone(t, r, "the old owner kept running after the TAKEOVER message")
	code, reason := h.WaitClosed()
	require.Equal(t, 4011, code)
	require.Equal(t, "SESSION_TAKEN_OVER", reason)
	require.LessOrEqual(t, len(h.Turns.Attempts()), turnWrites+1, "only the write in flight may still reach the database")
	require.Len(t, h.Written.Attempts(), eventWrites, "the old owner wrote a session event after the takeover")
	for _, a := range h.Turns.Attempts()[turnWrites:] {
		require.False(t, a.OK, "the fence refuses it")
	}
	pod, fence, ok := h.Store.Owner(h.SID)
	require.True(t, ok)
	require.Equal(t, "pod-b", pod, "the old owner did not release the new owner's key")
	require.Equal(t, newFence, fence)
	rt := rtOf(t, h)
	require.Equal(t, "pod-b", rt.PodID, "the old owner wrote its rt hash over the new owner's")
	require.Equal(t, 99, rt.Generation)
	require.Equal(t, float64(1), h.Metrics.FenceLostTotal())
	require.Equal(t, session.StatusActive, h.Session.Status(h.SID), "stopping is not ending the session")
	require.Zero(t, h.Mgr.Active())
}

func TestStaleTakeoverMessageIsIgnored(t *testing.T) {
	h := rtharness.New(t)
	started(t, h)
	fence := h.Session.Fence(h.SID)
	for _, f := range []int64{fence, fence - 1, 0} { // the message of our own fence or of an older one
		conversation.HandleTakeover(h.Mgr, rtstate.Takeover{SessionID: h.SID, Fence: f})
	}
	h.Sync()
	require.True(t, h.View().Connected)
	require.Equal(t, session.StatusActive, h.View().Status)
	// a message for a session this pod does not hold is nothing to do either
	conversation.HandleTakeover(h.Mgr, rtstate.Takeover{SessionID: uuid.New(), Fence: fence + 5})
	h.Sync()
	require.Equal(t, 1, h.Mgr.Active())
	// and a newer fence does stop it
	r, _ := h.Runtime()
	conversation.HandleTakeover(h.Mgr, rtstate.Takeover{SessionID: h.SID, Fence: fence + 1})
	waitDone(t, r, "a newer fence")
}

func TestTakeoverFencesOldOwner(t *testing.T) {
	h, gs := speakingSession(t)
	rA, _ := h.Runtime()
	oldGen := h.View().Generation
	oldFence := h.Session.Fence(h.SID)
	lastSeq := lastSeen(h)
	h.WaitFor("the turn in redis", func() bool {
		h.AdvanceClock(300 * time.Millisecond)
		h.Tick()
		_, ok, _ := h.Store.ReadTurn(t.Context(), h.SID)
		return ok
	})
	catchUp(t, h, true)
	rt := rtOf(t, h)
	entries, _ := h.Store.ReadEvents(t.Context(), h.SID)
	known := max(rt.LastServerSeq, entries[len(entries)-1].Seq)
	snapTurn, _, _ := h.Store.ReadTurn(t.Context(), h.SID)
	require.NotEmpty(t, snapTurn.AssistantText)

	b := h.Peer("pod-b")
	b.Connect(b.ResumeCmd(lastSeq))
	snapEv := b.WaitEvents("session.snapshot", 1)[0]
	b.Sync()

	// Pod A stops quickly and writes nothing more; the fence moved.
	waitDone(t, rA, "the old owner did not stop")
	code, _ := h.WaitClosed()
	require.Equal(t, 4011, code)
	require.Equal(t, oldFence+1, h.Session.Fence(h.SID))
	pod, fence, _ := h.Store.Owner(h.SID)
	require.Equal(t, "pod-b", pod)
	require.Equal(t, oldFence+1, fence)

	// Pod B continues from what A left: a generation beyond A's, the next turn number, the
	// numbering after A's last server_seq.
	v := b.View()
	require.Equal(t, oldGen+1, v.Generation, "max(rt.generation, maxGen)+1")
	require.Equal(t, 2, v.NextTurnSeq, "maxSeq+1")
	snap := snapEv.Payload.(protocol.SessionSnapshot)
	require.Equal(t, known+1, snapEv.ServerSeq, "max(rt.last_server_seq, stream tip)+1")
	require.Equal(t, snapEv.ServerSeq, snap.ServerSeqBaseline)
	require.Equal(t, oldGen+1, snap.ActiveGeneration)
	require.Equal(t, "COMPLETE", snap.Replay, "the client had everything A sent")
	require.Zero(t, snap.ReplayedCount)
	require.Equal(t, 2, b.Router.STTOpens())

	// The turn A left in progress is FAILED with what Redis had of it.
	flushed(t, b)
	row := finalRow(t, h, 1)
	require.Equal(t, session.TurnFailed, *row.Status)
	require.Equal(t, "TAKEOVER", *row.ErrorCode)
	require.Equal(t, snapTurn.AssistantText, *row.AssistantText)
	tf := sessionEventsOf(b, "TURN_FAILED")
	require.Len(t, tf, 1)
	require.Equal(t, row.ID, *tf[0].TurnID)
	require.Equal(t, "TAKEOVER", payloadOf(t, tf[0])["stage"])
	oc := sessionEventsOf(b, "OWNER_CHANGED")
	require.Len(t, oc, 2) // the start, and the takeover
	require.Equal(t, map[string]any{"reason": "TAKEOVER", "from_pod": "pod-a", "to_pod": "pod-b", "fence": float64(oldFence + 1)}, payloadOf(t, oc[1]))
	rc := sessionEventsOf(b, "SESSION_RECONNECTED")
	require.Len(t, rc, 1)
	require.Equal(t, false, payloadOf(t, rc[0])["same_pod"])
	require.Equal(t, 1, b.Mgr.Active())
	require.Zero(t, h.Mgr.Active())

	// Pod B serves the session.
	gs.limit.Store(1 << 40)
	b.Router.STT().SetFinalizeText(questionJA)
	b.Speak("ja_greeting_short")
	b.Silence(1200)
	fin := b.WaitEvents("stt.final", 1)[0]
	require.Equal(t, 2, *fin.TurnSeq)
	require.Equal(t, oldGen+2, *b.WaitEvents("ai.generation_started", 1)[0].Generation)
}

func TestTakeoverBaselines(t *testing.T) {
	for _, tc := range []struct {
		name       string
		rtGen      int
		rows       []int // generations of the turn rows (sequence 0, 1, …)
		wantGen    uint32
		wantNext   int
		rtLast     int64
		tip        int64
		wantSeqBas int64
	}{
		{"the turns are ahead of rt", 4, []int{3, 7}, 8, 2, 30, 33, 34},
		{"rt is ahead of the turns", 12, []int{3, 7}, 13, 2, 40, 33, 41},
		{"no turn yet: the opening has seq 0", 0, nil, 1, 1, 5, 0, 6},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := seededTakeover(t, 5700*time.Millisecond, func(rt *rtstate.RT) { rt.Generation, rt.LastServerSeq = tc.rtGen, tc.rtLast })
			for i, g := range tc.rows {
				seedTurn(t, h, i, g, session.TurnCompleted, "", "", "")
			}
			if tc.tip > 0 {
				require.NoError(t, h.Store.AppendEvent(t.Context(), h.SID, rtstate.EventEntry{Seq: tc.tip, PrevSeq: 28, Type: "turn.completed"}))
			}
			snapEv := resume(t, h, 0)
			h.Sync()
			v := h.View()
			require.Equal(t, tc.wantGen, v.Generation)
			require.Equal(t, tc.wantNext, v.NextTurnSeq)
			require.Equal(t, tc.wantSeqBas, snapEv.ServerSeq)
			require.Equal(t, tc.wantGen, snapEv.Payload.(protocol.SessionSnapshot).ActiveGeneration)
			require.Equal(t, tc.wantGen, *snapEv.Generation)
			// the time away: 5.7 s → 5 whole seconds
			flushed(t, h)
			require.Equal(t, 5, h.Session.Disconnected(h.SID))
		})
	}
}

func TestTakeoverRebuildsTheSessionState(t *testing.T) {
	h := seededTakeover(t, time.Second, func(rt *rtstate.RT) {
		rt.NextTurnSeq = 4
		rt.LastDurableSeq = 28
	})
	for i, u := range []string{"こんにちは", "趣味は読書です", "よく図書館に行きます"} {
		seedTurn(t, h, i+1, i+1, session.TurnCompleted, u, "そうですか。"+u+"ですね。", "そうですか。")
	}
	seedTurn(t, h, 4, 4, session.TurnInterrupted, "今日は", "長い答えです。まだ続きます。", "長い答えです。")
	seedTurn(t, h, 5, 5, session.TurnDiscarded, "", "", "")
	require.NoError(t, h.Store.WriteSummary(t.Context(), h.SID, rtstate.Summary{Text: "要約です。", CoveredUntilSeq: 2}, time.Hour))
	raw, err := json.Marshal(memory.MemoryContext{SessionID: h.SID, UserMemVer: 7, Core: []memory.ContextItem{{Content: "名前はタロウ"}}})
	require.NoError(t, err)
	h.Store.SetMemCtx(h.SID, raw)

	snap := resume(t, h, 0).Payload.(protocol.SessionSnapshot)
	h.Sync()

	// recent_turns: the final turns, oldest first, DISCARDED left out; an INTERRUPTED turn shows what was heard.
	require.Len(t, snap.RecentTurns, 4)
	for i, want := range []int{1, 2, 3, 4} {
		require.Equal(t, want, snap.RecentTurns[i].TurnSeq)
	}
	require.Equal(t, "INTERRUPTED", snap.RecentTurns[3].Status)
	require.Equal(t, "長い答えです。", *snap.RecentTurns[3].AssistantSpokenText)
	require.Equal(t, 6, snap.NextTurnSeq, "after the discarded turn too: maxSeq+1")
	require.Empty(t, h.Memory.Builds(), "the memory context the session started with is read from redis, not built again")
	require.Equal(t, 1, h.Router.STTOpens())
	require.Equal(t, 1, h.Mgr.Active())
	require.Equal(t, session.StatusActive, h.View().Status)

	// The session's deadlines follow expires_at, not the takeover: the end comes on time.
	r, _ := h.Runtime()
	h.AdvanceClock(600 * time.Second)
	h.Tick()
	waitDone(t, r, "the session did not expire on time after the takeover")
	require.Equal(t, session.StatusCompleted, h.Session.Status(h.SID))
}

func TestResumeAdaptiveFromRT(t *testing.T) {
	adapted := learner.AdaptiveDifficulty{AlgorithmVersion: 1, SpeechSpeed: 5, TTSSpeakingRate: 1.2, MaxAISentencesPerTurn: 2, InSessionAdjustments: 2}
	t.Run("rt overrides the snapshot of the session", func(t *testing.T) {
		h := seededTakeover(t, time.Second, func(rt *rtstate.RT) { rt.Adaptive, rt.TurnsSinceAdjust = adapted, 3 })
		resume(t, h, 0)
		r, _ := h.Runtime()
		ad, err := r.AdaptiveForTest(t.Context())
		require.NoError(t, err)
		require.Equal(t, adapted, ad)
	})
	t.Run("without it the snapshot of the session stands", func(t *testing.T) {
		h := seededTakeover(t, time.Second, nil)
		resume(t, h, 0)
		r, _ := h.Runtime()
		ad, err := r.AdaptiveForTest(t.Context())
		require.NoError(t, err)
		require.Equal(t, 3, ad.SpeechSpeed)
		require.Zero(t, ad.InSessionAdjustments)
	})
}

func TestTakeoverFailsTheTurnLeftInProgress(t *testing.T) {
	t.Run("redis knows the turn", func(t *testing.T) {
		h := seededTakeover(t, time.Second, nil)
		id := uuid.New()
		require.NoError(t, h.Turns.Insert(t.Context(), 1, conversation.TurnStart{ID: id, SessionID: h.SID, Sequence: 3, CreatedAt: rtharness.T0}))
		require.NoError(t, h.Store.WriteTurn(t.Context(), h.SID, rtstate.TurnSnap{TurnID: id, TurnSeq: 3, Initiator: "USER", AssistantText: "途中まで"}, time.Hour))
		resume(t, h, 0)
		h.Sync()
		flushed(t, h)
		row := finalRow(t, h, 3)
		require.Equal(t, session.TurnFailed, *row.Status)
		require.Equal(t, "途中まで", *row.AssistantText)
		require.Equal(t, "TAKEOVER", *row.ErrorCode)
		tf := sessionEventsOf(h, "TURN_FAILED")
		require.Len(t, tf, 1)
		require.Equal(t, id, *tf[0].TurnID)
		require.Equal(t, map[string]any{"turn_seq": float64(3), "stage": "TAKEOVER", "error_class": "TAKEOVER"}, payloadOf(t, tf[0]))
		require.Equal(t, 4, h.View().NextTurnSeq)
	})
	t.Run("nothing was in progress", func(t *testing.T) {
		h := seededTakeover(t, time.Second, nil)
		seedTurn(t, h, 1, 1, session.TurnCompleted, "こんにちは", "はい。", "はい。")
		resume(t, h, 0)
		h.Sync()
		flushed(t, h)
		require.Empty(t, sessionEventsOf(h, "TURN_FAILED"))
	})
}

func TestTakeoverBeyondWindow(t *testing.T) {
	// 300 s into the session, the client was last heard at 260 s: 40 s away.
	h := seededTakeover(t, 0, func(rt *rtstate.RT) { rt.DisconnectedAtMS = rtharness.T0.Add(260 * time.Second).UnixMilli() })
	h.Clock.Set(rtharness.T0.Add(300 * time.Second))
	h.Connect(h.ResumeCmd(0))
	require.NoError(t, h.AttachResult())
	code, _ := h.WaitClosed()
	require.Equal(t, 1000, code)

	requireOrder(t, h.Calls.Calls(), "ClaimRuntime", "AddDisconnected", "BeginEnding", "Complete")
	require.Equal(t, 40, h.Session.Disconnected(h.SID))
	require.Equal(t, session.StatusCompleted, h.Session.Status(h.SID))
	ending := h.Events("session.ending")
	require.Len(t, ending, 1)
	require.Equal(t, "ENDING", ending[0].Payload.(protocol.SessionEnding).Stage)
	require.Equal(t, "RECONNECT_TIMEOUT", ending[0].Payload.(protocol.SessionEnding).Reason)
	done := h.Events("session.completed")
	require.Len(t, done, 1)
	sc := done[0].Payload.(protocol.SessionCompleted)
	require.Equal(t, "RECONNECT_TIMEOUT", sc.EndReason)
	require.Equal(t, 40, sc.DisconnectedSeconds)
	require.Equal(t, 260, sc.ChargedSeconds, "300 s elapsed, 40 s away")
	require.Greater(t, done[0].ServerSeq, int64(30), "the numbering continues the previous owner's")
	require.Zero(t, h.Router.STTOpens(), "no stream for a session that is over")
	require.Zero(t, h.Mgr.Active())
}

func TestTakeoverBeyondWindowFailsTheTurnLeftInProgress(t *testing.T) {
	h := seededTakeover(t, 31*time.Second, nil)
	id := uuid.New()
	require.NoError(t, h.Turns.Insert(t.Context(), 1, conversation.TurnStart{ID: id, SessionID: h.SID, Sequence: 1, CreatedAt: rtharness.T0}))
	h.Connect(h.ResumeCmd(0))
	require.NoError(t, h.AttachResult())
	row := finalRow(t, h, 1)
	require.Equal(t, session.TurnFailed, *row.Status, "a turn left without a status must not stay that way")
}

func TestTakeoverWithinWindowJustInside(t *testing.T) {
	h := seededTakeover(t, 30*time.Second, nil) // exactly RECONNECT_WINDOW: not "more than"
	snap := resume(t, h, 0)
	require.NotNil(t, snap)
	require.Equal(t, session.StatusActive, h.Session.Status(h.SID))
	require.Equal(t, 1, h.Mgr.Active())
}

func TestTakeoverRedisDown(t *testing.T) {
	h := seededTakeover(t, time.Second, nil)
	seedTurn(t, h, 1, 8, session.TurnCompleted, "こんにちは", "はい。", "はい。")
	seedTurn(t, h, 3, 9, session.TurnCompleted, "趣味は", "読書です。", "読書です。")
	h.Store.FailAlways(errors.New("redis down"))

	snapEv := resume(t, h, 17)
	h.Sync()
	snap := snapEv.Payload.(protocol.SessionSnapshot)
	require.Equal(t, "UNAVAILABLE", snap.Replay, "nothing to replay from")
	require.Zero(t, snap.ReplayedCount)
	require.Equal(t, uint32(10), snap.ActiveGeneration, "max(session_turns.generation)+1")
	require.Equal(t, 4, snap.NextTurnSeq)
	require.Equal(t, int64(18), snapEv.ServerSeq, "the numbering continues from what the client has")
	require.Len(t, snap.RecentTurns, 2)
	require.Len(t, h.Memory.Builds(), 1, "no memory context in redis: it is built again")
	flushed(t, h)
	require.Zero(t, h.Session.Disconnected(h.SID), "when the time away is unknown nothing is deducted")
	require.NotContains(t, h.Calls.Calls(), "AddDisconnected")
	require.Equal(t, session.StatusActive, h.View().Status)
	require.Equal(t, 1, h.Router.STTOpens())
	rc := sessionEventsOf(h, "SESSION_RECONNECTED")
	require.Len(t, rc, 1)
	require.Equal(t, "UNAVAILABLE", payloadOf(t, rc[0])["replay"])
}

// TestFinalizedTurnsCarryTheGeneration: the runtime writes the highest generation it had issued
// into every turn it closes, so a takeover that cannot read rt (§7.9.4) still continues above it.
func TestFinalizedTurnsCarryTheGeneration(t *testing.T) {
	h := rtharness.New(t)
	started(t, h)
	answerTurn(t, h, questionJA, 0)
	answerTurn(t, h, questionJA, 1)
	flushed(t, h)
	gens := h.Events("ai.generation_started")
	require.Len(t, gens, 2)
	for i, g := range gens {
		row := finalRow(t, h, i+1)
		require.Equal(t, int(*g.Generation), row.Generation)
	}
}

// TestTakeoverRedisDownAfterARealSession: with redis gone the generation comes from the turns the
// previous owner closed, and it is above everything that owner issued.
func TestTakeoverRedisDownAfterARealSession(t *testing.T) {
	h := rtharness.New(t)
	started(t, h)
	answerTurn(t, h, questionJA, 0)
	answerTurn(t, h, questionJA, 1)
	flushed(t, h)
	issued := h.View().Generation
	lastSeq := lastSeen(h)
	h.Store.FailAlways(errors.New("redis down"))

	b := h.Peer("pod-b")
	b.Connect(b.ResumeCmd(lastSeq))
	snap := b.WaitEvents("session.snapshot", 1)[0].Payload.(protocol.SessionSnapshot)
	require.Equal(t, "UNAVAILABLE", snap.Replay)
	require.Equal(t, issued+1, snap.ActiveGeneration, "a generation the client's filter on frames of lower ones cannot drop")
	require.Equal(t, 3, snap.NextTurnSeq)
	require.Len(t, snap.RecentTurns, 2)
}

func TestTakeoverRefusals(t *testing.T) {
	t.Run("the session is not ACTIVE any more", func(t *testing.T) {
		h := seededTakeover(t, time.Second, nil)
		h.Session.FailNext("ClaimRuntime", session.ErrInvalidTransition) // 0 rows: ended between the check and the claim
		h.Connect(h.ResumeCmd(0))
		require.Error(t, h.AttachResult())
		code, _ := h.WaitClosed()
		require.Equal(t, 4010, code)
		require.Zero(t, h.Mgr.Active(), "the slot was given back")
	})
	t.Run("the pod is at the hard limit", func(t *testing.T) {
		h := seededTakeover(t, time.Second, nil, rtharness.WithLimits(conversation.Limits{ReadyMax: 1, Max: 1, HardMax: 0}))
		h.Connect(h.ResumeCmd(0))
		require.Error(t, h.AttachResult())
		code, _ := h.WaitClosed()
		require.Equal(t, 4503, code)
		require.NotContains(t, h.Calls.Calls(), "ClaimRuntime", "a refused resume does not take the session")
	})
	t.Run("a resume may exceed the limit of new starts", func(t *testing.T) {
		h := seededTakeover(t, time.Second, nil, rtharness.WithLimits(conversation.Limits{ReadyMax: 1, Max: 0, HardMax: 1}))
		h.Connect(h.ResumeCmd(0))
		h.WaitEvents("session.snapshot", 1)
		require.Equal(t, 1, h.Mgr.Active())
	})
}

func TestTakeoverSTTFailureEndsSession(t *testing.T) {
	h := seededTakeover(t, time.Second, nil, rtharness.WithSTTOpenError(provider.NewError(provider.FakePrimary, provider.CapSTT, provider.KindUnavailable, "no stream")))
	h.Connect(h.ResumeCmd(0))
	failed := h.WaitEvents("session.failed", 1)
	require.Equal(t, "PROVIDER_FAILURE", failed[0].Payload.(protocol.SessionFailed).EndReason)
	code, _ := h.WaitClosed()
	require.Equal(t, 4500, code)
	require.Equal(t, session.StatusFailed, h.Session.Status(h.SID))
}

func TestTakeoverBudget3s(t *testing.T) {
	h := seededTakeover(t, time.Second, nil)
	h.Gates.Config = make(chan struct{}) // the config load hangs
	h.Connect(h.ResumeCmd(0))
	h.WaitFor("the takeover to wait for the config", func() bool { return h.Blocked() == 1 })
	h.AdvanceClock(2900 * time.Millisecond)
	h.Tick()
	h.Sync()
	require.Equal(t, session.StatusActive, h.Session.Status(h.SID))
	h.AdvanceClock(200 * time.Millisecond)
	h.Tick()
	failed := h.WaitEvents("session.failed", 1)
	require.Equal(t, "SERVER_ERROR", failed[0].Payload.(protocol.SessionFailed).EndReason)
	code, _ := h.WaitClosed()
	require.Equal(t, 1011, code)
}

func TestTakeoverRefusesTurnsUntilReady(t *testing.T) {
	h := seededTakeover(t, time.Second, nil)
	h.Gates.Config = make(chan struct{})
	h.Connect(h.ResumeCmd(0))
	h.WaitFor("the takeover to wait for the config", func() bool { return h.Blocked() == 1 })
	h.Send(h.EndCmd()) // session.end while the takeover loads waits like it does while STARTING
	h.Sync()
	require.True(t, h.View().PendingEnd)
	close(h.Gates.Config)
	r, _ := h.Runtime()
	waitDone(t, r, "the pending end ran after the takeover")
	require.Equal(t, session.StatusCompleted, h.Session.Status(h.SID))
}

func TestPodChannelResubscribes(t *testing.T) {
	h := rtharness.New(t)
	store := fakes.NewRuntimeStore()
	store.FailNext("SubscribePod", errors.New("redis down"))
	deps := h.Deps
	deps.Store, deps.PodSubscribeBackoff = store, time.Millisecond
	m := conversation.NewManager(deps)
	t.Cleanup(m.Close)
	h.WaitFor("the subscriber to subscribe again", func() bool { return store.Subscribers("pod-a") == 1 })
	require.Equal(t, 1, h.Store.Subscribers("pod-a"), "the manager of the harness subscribed when it was built")
}

// TestReconnectWindowStartsWhenTheLossIsNoticed: the 30 s run from the moment the runtime notices
// (§7.10.1 reconnectTimer), not from the client's last activity, which may be long before.
func TestReconnectWindowStartsWhenTheLossIsNoticed(t *testing.T) {
	h := rtharness.New(t)
	started(t, h)
	r, _ := h.Runtime()
	h.AdvanceClock(10 * time.Second) // the client says nothing for 10 s, the connection is not noticed dead yet
	detach(t, h)
	h.AdvanceClock(25 * time.Second)
	h.Tick()
	h.Sync()
	require.Equal(t, session.StatusActive, h.View().Status, "35 s after the last activity but 25 s after the loss")
	h.AdvanceClock(5 * time.Second)
	h.Tick()
	waitDone(t, r, "30 s after the loss")
	require.Equal(t, 40, h.Session.Disconnected(h.SID), "the time away counts from the last activity")
}

// TestResumeCancelsTheReconnectTimer: a client that came back is not timed out by the window of the
// disconnect before it.
func TestResumeCancelsTheReconnectTimer(t *testing.T) {
	h := rtharness.New(t)
	started(t, h)
	lastSeq := lastSeen(h)
	detach(t, h)
	h.AdvanceClock(10 * time.Second)
	resume(t, h, lastSeq)
	h.Sync()
	h.AdvanceClock(25 * time.Second) // 35 s after the loss: the old window has passed
	h.Tick()
	h.Sync()
	require.Equal(t, session.StatusActive, h.View().Status)
	require.True(t, h.View().Connected)
}
