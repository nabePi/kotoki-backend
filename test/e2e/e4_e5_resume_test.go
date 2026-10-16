//go:build e2e

package e2e

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/conversation/protocol"
	"github.com/nabePi/kotoki-backend/internal/kotokiclient"
)

// midAnswer is how long a script waits after its last speech frame before it breaks the
// connection: past the end-of-turn silence (500 ms) and the first audio of the answer (about
// 1.1 s later), well inside the shortest fake answer (1.7 s of audio).
const midAnswer = 2200 * time.Millisecond

// TestE4Reconnect is E4 (spec 14.5): the transport breaks while the AI speaks; the client comes back
// with a new ticket and session.resume, gets what it missed once, and the session goes on.
func TestE4Reconnect(t *testing.T) {
	u := newUser(t)
	s := startSession(t, u, "USER_FIRST")
	rec := newRecorder()
	rep, err := converse(t, u, s, []kotokiclient.Step{
		kotokiclient.Speak{Packets: packets(t, "ja_question"), Label: "ja_question"},
		kotokiclient.Think{D: midAnswer},
		kotokiclient.Drop{For: 3 * time.Second},
		kotokiclient.Speak{Packets: packets(t, "ja_greeting_short"), Label: "ja_greeting_short"},
		kotokiclient.WaitAIDone{},
		kotokiclient.End{},
	}, rec)
	require.NoError(t, err)
	require.Equal(t, "COMPLETED", rep.Status)
	require.Len(t, rep.ResumeMS, 1)

	// The resume: a session.snapshot with a complete replay, preceded by the events the client
	// missed (flagged replay), none of which it had already seen.
	types := rec.types()
	inOrder(t, types, []string{"session.started", "ai.audio_started", "session.snapshot"})
	rec.mu.Lock()
	events := append([]protocol.ServerEvent(nil), rec.events...)
	rec.mu.Unlock()
	snapAt, maxBefore, replayed := -1, int64(0), 0
	seen := map[int64]bool{}
	for i, e := range events {
		// session.ready is sent before the runtime exists and always carries seq 1: it sits outside
		// the numbered stream the client resumes from.
		if e.Type == "session.ready" {
			continue
		}
		assert.Falsef(t, seen[e.ServerSeq], "server_seq %d (%s) delivered twice", e.ServerSeq, e.Type)
		seen[e.ServerSeq] = true
		switch {
		case e.Type == "session.snapshot" && snapAt < 0:
			snapAt = i
			assert.Equal(t, "COMPLETE", e.Payload.(protocol.SessionSnapshot).Replay)
		case snapAt < 0 && e.Replay:
			replayed++
			assert.Greater(t, e.ServerSeq, maxBefore, "a replayed event is one the client had not seen (%s)", e.Type)
		case snapAt < 0:
			maxBefore = max(maxBefore, e.ServerSeq)
		}
	}
	require.GreaterOrEqual(t, snapAt, 0)
	t.Logf("E4 events %v; %d replayed before the snapshot", types, replayed)

	// The turn that was running when the line broke is FAILED; the next one completes.
	eventually(t, func() bool {
		r := turnRows(t, s.ID)
		return len(r) == 2 && r[0].Status != nil && r[1].Status != nil
	}, "two turns persisted")
	turns := turnRows(t, s.ID)
	assert.Equal(t, "FAILED", str(turns[0].Status), "the turn in progress when the transport broke")
	assert.Equal(t, "COMPLETED", str(turns[1].Status))

	got := waitSession(t, s.ID, "COMPLETED")
	assert.Equal(t, "USER_ENDED", str(got.EndReason))
	assert.GreaterOrEqual(t, got.Disconnected, 2, "disconnected_seconds of a 3 s absence")
	ev := sessionEvents(t, s.ID)
	assert.NotEmpty(t, ev["SESSION_DISCONNECTED"])
	assert.NotEmpty(t, ev["SESSION_RECONNECTED"])
	assert.NotEmpty(t, ev["TURN_FAILED"])
}

// TestE5TakeoverAcrossPods is E5 (spec 14.5): two realtime processes; the owner of a session is
// killed with SIGKILL while the AI speaks; the client reconnects to the other process, which takes
// the session over (D-084). The turn in progress is FAILED and the session goes on.
func TestE5TakeoverAcrossPods(t *testing.T) {
	podA, podB := extraRealtime(t, "e2e-pod-5a"), extraRealtime(t, "e2e-pod-5b")
	u := newUser(t)
	s := startSession(t, u, "USER_FIRST")

	// On pod A: speak, and the moment the AI begins to answer, the pod dies.
	first := newRecorder()
	killed := false
	sink := kotokiclient.SinkFuncs{
		Event: func(e kotokiclient.Received[protocol.ServerEvent]) {
			first.OnEvent(e)
			if e.Value.Type == "ai.audio_started" && !killed {
				killed = true
				_ = podA.cmd.Process.Kill()
			}
		},
		Audio: first.OnAudio,
	}
	_, err := converseOn(t, wsURLOf(podA), u, s, []kotokiclient.Step{
		kotokiclient.Speak{Packets: packets(t, "ja_question"), Label: "ja_question"},
		kotokiclient.WaitAIDone{},
	}, sink)
	require.Error(t, err, "the owner is gone: the first conversation cannot finish")
	require.True(t, killed, "the AI had begun to answer when the pod was killed")
	select {
	case werr := <-podA.exited:
		podA.exited <- werr
		require.Error(t, werr, "pod A was killed")
		assert.Contains(t, werr.Error(), "killed")
	case <-time.After(eventuallyWait):
		t.Fatal("pod A did not exit")
	}
	assert.Equal(t, "ACTIVE", readSession(t, s.ID).Status, "nobody has ended the session")

	// On pod B: the same session, the same user; B takes over and the conversation goes on.
	second := newRecorder()
	rep, err := converseOn(t, wsURLOf(podB), u, s, []kotokiclient.Step{
		kotokiclient.Speak{Packets: packets(t, "ja_greeting_short"), Label: "ja_greeting_short"},
		kotokiclient.WaitAIDone{},
		kotokiclient.End{},
	}, second)
	require.NoError(t, err)
	require.Equal(t, "COMPLETED", rep.Status)
	inOrder(t, second.types(), []string{"session.snapshot", "turn.started", "ai.audio_started", "turn.completed", "session.completed"})

	eventually(t, func() bool {
		r := turnRows(t, s.ID)
		return len(r) == 2 && r[0].Status != nil && r[1].Status != nil
	}, "two turns persisted")
	turns := turnRows(t, s.ID)
	assert.Equal(t, "FAILED", str(turns[0].Status), "the turn that pod A was running")
	assert.Equal(t, "COMPLETED", str(turns[1].Status), "the turn pod B ran")

	got := waitSession(t, s.ID, "COMPLETED")
	assert.Equal(t, "USER_ENDED", str(got.EndReason))
	assert.GreaterOrEqual(t, got.Fence, int64(2), "the claim of pod B raised the fence past pod A's")
	changed := sessionEvents(t, s.ID)["OWNER_CHANGED"]
	require.Len(t, changed, 2, "START on pod A, TAKEOVER by pod B: %v", changed)
	assert.Equal(t, map[string]any{"fence": 1.0, "from_pod": "", "reason": "START", "to_pod": "e2e-pod-5a"}, changed[0])
	assert.Equal(t, map[string]any{"fence": 2.0, "from_pod": "e2e-pod-5a", "reason": "TAKEOVER", "to_pod": "e2e-pod-5b"}, changed[1])
}
