//go:build e2e

package e2e

import (
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/conversation/protocol"
	"github.com/nabePi/kotoki-backend/internal/kotokiclient"
)

// TestE10DurationElapsed is E10 (spec 14.5): a 300 s session runs out. It takes the whole 300 s of
// real time, so it only runs with E2E_LONG=1.
func TestE10DurationElapsed(t *testing.T) {
	if os.Getenv("E2E_LONG") != "1" {
		t.Skip("E10 lasts 300 s of real time: set E2E_LONG=1")
	}
	u := newUser(t)
	s := startSession(t, u, "USER_FIRST") // 300 s
	rec := newRecorder()
	// The mic stays open and quiet; the script ends by itself when the server completes the session.
	rep, err := converse(t, u, s, []kotokiclient.Step{kotokiclient.Think{D: 8 * time.Minute}}, rec)
	require.NoError(t, err)
	require.Equal(t, "COMPLETED", rep.Status)

	var stages []protocol.SessionEnding
	for _, e := range rec.of("session.ending") {
		stages = append(stages, e.Payload.(protocol.SessionEnding))
	}
	require.Len(t, stages, 3, "WARNING at 60 s, WARNING at 10 s, then ENDING: %+v", stages)
	assert.Equal(t, "WARNING", stages[0].Stage)
	assert.Equal(t, 60, stages[0].RemainingSeconds)
	assert.Equal(t, "WARNING", stages[1].Stage)
	assert.Equal(t, 10, stages[1].RemainingSeconds)
	assert.Equal(t, "ENDING", stages[2].Stage)
	assert.Equal(t, "DURATION_ELAPSED", stages[2].Reason)
	inOrder(t, rec.types(), []string{"session.started", "session.ending", "session.ending", "session.ending", "session.completed"})

	got := waitSession(t, s.ID, "COMPLETED")
	assert.Equal(t, "DURATION_ELAPSED", str(got.EndReason))
	assert.LessOrEqual(t, got.Charged, 300, "charged_seconds is never more than the reservation")
	assert.Equal(t, got.Charged, rep.ChargedSeconds)
	t.Logf("E10 charged_seconds=%d", got.Charged)
}

// idleBeforeDrain is how long the session is ACTIVE before its pod is told to stop, so the charge
// up to the handoff is a few seconds that can be told from the 30 s that follow.
const idleBeforeDrain = 3 * time.Second

// drained runs a script on pod and stops the pod (SIGTERM) idleBeforeDrain after the session has
// started: the session is idle (LISTENING), so the pod hands it off at once with error
// SERVER_DRAINING and close 4100 (spec 7.12.1).
func drained(t *testing.T, pod *proc, u *user, s kotokiclient.Session) (kotokiclient.Report, *recorder, error) {
	t.Helper()
	rec := newRecorder()
	sink := kotokiclient.SinkFuncs{
		Event: func(e kotokiclient.Received[protocol.ServerEvent]) {
			rec.OnEvent(e)
			if e.Value.Type == "session.started" {
				time.AfterFunc(idleBeforeDrain, func() { pod.signal(syscall.SIGTERM) })
			}
		},
		Audio: rec.OnAudio,
	}
	rep, err := converseOn(t, wsURLOf(pod), u, s, []kotokiclient.Step{kotokiclient.Think{D: time.Minute}}, sink)
	return rep, rec, err
}

// TestE14HandoffDrain is E14 (spec 14.5, S-10): a pod that is stopped hands its session off with
// close 4100 and sessions.handoff_at. A client that does not come back within 30 s loses the
// session to the sweeper, which bills up to the handoff only; one that does come back keeps it.
func TestE14HandoffDrain(t *testing.T) {
	t.Run("not resumed", func(t *testing.T) {
		pod := extraRealtime(t, "e2e-pod-14a")
		u := newUser(t)
		s := startSession(t, u, "USER_FIRST")
		rep, rec, err := drained(t, pod, u, s)
		var se *kotokiclient.ServerError
		require.ErrorAs(t, err, &se, "the drain ends the conversation with a fatal error event")
		assert.Equal(t, "SERVER_DRAINING", se.Code)
		assert.True(t, se.Retryable)
		assert.Equal(t, 4100, rep.CloseCode)
		assert.Equal(t, 1, len(rec.of("session.started")), "the client did not reconnect")

		// The handoff is recorded and the session stays ACTIVE for whoever resumes it.
		eventually(t, func() bool { return readSession(t, s.ID).HandoffAt != nil }, "handoff_at set")
		got := readSession(t, s.ID)
		assert.Equal(t, "ACTIVE", got.Status)
		handoff := *got.HandoffAt

		// 31 s without a taker (RECONNECT_WINDOW is 30 s): the sweeper ends it. The only wait here
		// is the one the spec names.
		require.Eventually(t, func() bool { return time.Since(handoff) > 31*time.Second }, 40*time.Second, eventuallyTick, "31 s since the handoff")
		_, err = newSweeper().Tick(ctxT(t, 30*time.Second))
		require.NoError(t, err)
		done := waitSession(t, s.ID, "COMPLETED")
		assert.Equal(t, "RECONNECT_TIMEOUT", str(done.EndReason))
		require.NotNil(t, done.EndingAt)
		require.NotNil(t, done.EndedAt)
		assert.True(t, done.EndingAt.Equal(handoff), "billing ends at the handoff: ending_at=%s handoff_at=%s", done.EndingAt, handoff)
		assert.Greater(t, done.EndedAt.Sub(handoff), 30*time.Second, "the session was closed well after the handoff")
		assert.GreaterOrEqual(t, handoff.Sub(*done.StartedAt), idleBeforeDrain-500*time.Millisecond)
		assert.InDelta(t, handoff.Sub(*done.StartedAt).Seconds(), float64(done.Charged), 1.0, "charged_seconds counts start..handoff_at only")
		assert.Less(t, done.Charged, 10, "nothing after the handoff is billed")
		t.Logf("E14 charged_seconds=%d (start..handoff %.1fs, start..ended %.1fs)", done.Charged, handoff.Sub(*done.StartedAt).Seconds(), done.EndedAt.Sub(*done.StartedAt).Seconds())
	})

	t.Run("resumed in time", func(t *testing.T) {
		pod := extraRealtime(t, "e2e-pod-14b")
		u := newUser(t)
		s := startSession(t, u, "USER_FIRST")
		rep, _, err := drained(t, pod, u, s)
		var se *kotokiclient.ServerError
		require.ErrorAs(t, err, &se)
		assert.Equal(t, "SERVER_DRAINING", se.Code)
		assert.Equal(t, 4100, rep.CloseCode)
		eventually(t, func() bool { return readSession(t, s.ID).HandoffAt != nil }, "handoff_at set")

		// Within the window the client reconnects to the process that is still up.
		rec := newRecorder()
		rep2, err := converse(t, u, s, []kotokiclient.Step{
			kotokiclient.Speak{Packets: packets(t, "ja_greeting_short"), Label: "ja_greeting_short"},
			kotokiclient.WaitAIDone{},
			kotokiclient.End{},
		}, rec)
		require.NoError(t, err)
		require.Equal(t, "COMPLETED", rep2.Status)
		inOrder(t, rec.types(), []string{"session.snapshot", "turn.started", "ai.audio_started", "turn.completed", "session.completed"})
		got := waitSession(t, s.ID, "COMPLETED")
		assert.Equal(t, "USER_ENDED", str(got.EndReason))
		assert.Nil(t, got.HandoffAt, "the new owner's claim clears handoff_at")
		eventually(t, func() bool { r := turnRows(t, s.ID); return len(r) == 1 && r[0].Status != nil }, "turn persisted")
		assert.Equal(t, "COMPLETED", str(turnRows(t, s.ID)[0].Status), "a drain does not fail turns")
	})
}
