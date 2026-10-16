//go:build e2e

package e2e

import (
	"encoding/json"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	amqp091 "github.com/rabbitmq/amqp091-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/conversation/protocol"
	"github.com/nabePi/kotoki-backend/internal/events"
	"github.com/nabePi/kotoki-backend/internal/kotokiclient"
	"github.com/nabePi/kotoki-backend/internal/redisstore"
	"github.com/nabePi/kotoki-backend/internal/testutil"
)

// sessionCompletedMessages pops every message of queue and returns the envelopes about sessionID.
// The e2e database is shared by the whole suite, so a worker may publish the events of other
// sessions to the same queue; they are not what these scenarios count.
func sessionCompletedMessages(t *testing.T, rabbitURL, queue string, sessionID uuid.UUID) []events.Envelope {
	t.Helper()
	conn, err := amqp091.Dial(rabbitURL)
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()
	ch, err := conn.Channel()
	require.NoError(t, err)
	var out []events.Envelope
	for {
		d, ok, err := ch.Get(queue, true)
		require.NoError(t, err)
		if !ok {
			return out
		}
		var env events.Envelope
		require.NoError(t, json.Unmarshal(d.Body, &env))
		if env.AggregateID == sessionID {
			out = append(out, env)
		}
	}
}

func outboxCount(t *testing.T, sessionID uuid.UUID) (total, published int) {
	t.Helper()
	require.NoError(t, stack.pool.QueryRow(t.Context(), `SELECT count(*), count(*) FILTER (WHERE status = 'PUBLISHED')
		FROM event_outbox WHERE aggregate_id = $1 AND event_type = 'session.completed'`, sessionID).Scan(&total, &published))
	return total, published
}

// TestM6_PodKill_SweeperCompletesOnce is E5 as a pod kill with a worker (spec 9.9.2, review focus 1):
// the only realtime process holding an ACTIVE session is killed with SIGKILL. Its heartbeat stops;
// the worker's sweeper fails the session once, at the last heartbeat, bills the time up to it less
// the 30 s service-fault credit, and the outbox publisher announces session.completed once.
//
// The plan ran `worker run-job session_sweeper` every 10 s; each run is a new process whose
// heartbeat guard never reaches 45 s of healthy Redis, so rule (b) would never fire. A running
// worker (--handlers=sweeper,outbox) is what a pod is (deviation M6-T24).
func TestM6_PodKill_SweeperCompletesOnce(t *testing.T) {
	rabbitURL := testutil.NewRabbitMQ(t)
	worker := stack.startWorker(t, rabbitURL, "sweeper,outbox")
	t.Cleanup(worker.stop)
	pod := extraRealtime(t, "e2e-pod-m6-kill")
	u := newUser(t)
	s := startSession(t, u, "USER_FIRST")

	// Stay on the line for 70 s, so the time up to the last heartbeat is past the 60 s below which a
	// service fault is free, then the pod dies. The mic stays open and quiet.
	var killTimer *time.Timer
	sink := kotokiclient.SinkFuncs{Event: func(e kotokiclient.Received[protocol.ServerEvent]) {
		if e.Value.Type == "session.started" && killTimer == nil {
			killTimer = time.AfterFunc(70*time.Second, func() { _ = pod.cmd.Process.Kill() })
		}
	}}
	_, err := converseOn(t, wsURLOf(pod), u, s, []kotokiclient.Step{kotokiclient.Think{D: 3 * time.Minute}}, sink)
	require.Error(t, err, "the pod is gone: the conversation cannot finish")
	select {
	case werr := <-pod.exited:
		pod.exited <- werr
		require.Error(t, werr)
		assert.Contains(t, werr.Error(), "killed")
	case <-time.After(eventuallyWait):
		t.Fatal("the pod did not exit")
	}
	require.Equal(t, "ACTIVE", readSession(t, s.ID).Status, "nobody has ended the session yet")

	raw, err := stack.rdb.Get(t.Context(), redisstore.HeartbeatKey(s.ID)).Result()
	require.NoError(t, err, "the pod beat before it died")
	lastMS, err := strconv.ParseInt(raw, 10, 64)
	require.NoError(t, err)
	lastBeat := time.UnixMilli(lastMS).UTC()
	killedAt := time.Now()

	// 45 s of silence plus the sweeper tick (2 s here, 10 s in production), and a margin.
	require.Eventually(t, func() bool { return readSession(t, s.ID).Status != "ACTIVE" }, 150*time.Second, 500*time.Millisecond,
		"the sweeper settles the session")
	t.Logf("settled %.0f s after the kill", time.Since(killedAt).Seconds())

	got := readSession(t, s.ID)
	require.Equal(t, "FAILED", got.Status)
	assert.Equal(t, "SERVER_ERROR", str(got.EndReason))
	require.NotNil(t, got.EndedAt)
	require.NotNil(t, got.StartedAt)
	assert.WithinDuration(t, lastBeat, *got.EndedAt, time.Millisecond, "the session ends at its last heartbeat, not when the sweeper noticed")
	activeMS := got.EndedAt.Sub(*got.StartedAt).Milliseconds() - int64(got.Disconnected)*1000
	require.GreaterOrEqual(t, activeMS, int64(60_000), "the scenario must be past the free threshold")
	wantCharged := int((activeMS+999)/1000) - 30
	assert.Equal(t, wantCharged, got.Charged, "active time up to the last heartbeat, less the 30 s credit; nothing after it")
	assert.Less(t, got.Charged, int(killedAt.Sub(*got.StartedAt).Seconds()), "the dead time is not billed")

	// exactly one event, published once to each session queue
	total, _ := outboxCount(t, s.ID)
	require.Equal(t, 1, total)
	require.Eventually(t, func() bool { _, pub := outboxCount(t, s.ID); return pub == 1 }, 20*time.Second, 200*time.Millisecond, "the outbox publisher sends it")
	var final, producer string
	require.NoError(t, stack.pool.QueryRow(t.Context(),
		`SELECT payload->>'final_status', producer FROM event_outbox WHERE aggregate_id = $1`, s.ID).Scan(&final, &producer))
	assert.Equal(t, "FAILED", final)
	assert.Equal(t, "WORKER", producer)
	for _, q := range []string{events.QueueEvaluation, events.QueueMemory, events.QueueSummary} {
		msgs := sessionCompletedMessages(t, rabbitURL, q, s.ID)
		require.Len(t, msgs, 1, q)
		assert.Equal(t, events.SessionCompleted, msgs[0].EventType)
	}

	// ledger: RESERVE, CONSUME, RELEASE; no HELD reservation; R1-R5 clean
	charged := int64(got.Charged)
	ledger := ledgerRows(t, s.ID)
	require.Len(t, ledger, 3, "RESERVE, CONSUME, RELEASE: %+v", ledger)
	assert.Equal(t, ledgerRow{"RESERVE", -300, 300}, ledger[0])
	assert.Equal(t, ledgerRow{"CONSUME", 0, -charged}, ledger[1])
	assert.Equal(t, ledgerRow{"RELEASE", 300 - charged, -(300 - charged)}, ledger[2])
	var held int
	require.NoError(t, stack.pool.QueryRow(t.Context(), `SELECT count(*) FROM balance_reservations WHERE session_id = $1 AND status = 'HELD'`, s.ID).Scan(&held))
	assert.Zero(t, held)
	testutil.AssertReconciled(t, stack.pool)

	// the sweeper does not touch it again: still one event, still one message
	time.Sleep(6 * time.Second)
	total, _ = outboxCount(t, s.ID)
	assert.Equal(t, 1, total)
	for _, q := range []string{events.QueueEvaluation, events.QueueMemory, events.QueueSummary} {
		assert.Empty(t, sessionCompletedMessages(t, rabbitURL, q, s.ID), q)
	}
}

// TestM6_ClientDisconnect_NoCharge: the client's connection breaks and stays broken past the 30 s
// reconnect window. The runtime ends the session RECONNECT_TIMEOUT (COMPLETED), the time away is
// not charged, and the worker publishes one session.completed.
func TestM6_ClientDisconnect_NoCharge(t *testing.T) {
	rabbitURL := testutil.NewRabbitMQ(t)
	worker := stack.startWorker(t, rabbitURL, "outbox")
	t.Cleanup(worker.stop)
	u := newUser(t)
	s := startSession(t, u, "USER_FIRST")

	// 2 s on the line, then the transport breaks for 40 s; the client is not back when the window
	// runs out, so its resume fails and the script ends with an error.
	_, err := converse(t, u, s, []kotokiclient.Step{
		kotokiclient.Think{D: 2 * time.Second},
		kotokiclient.Drop{For: 40 * time.Second},
		kotokiclient.End{},
	}, newRecorder())
	t.Logf("the conversation ended with: %v", err)

	got := waitSession(t, s.ID, "COMPLETED")
	assert.Equal(t, "RECONNECT_TIMEOUT", str(got.EndReason))
	require.NotNil(t, got.StartedAt)
	require.NotNil(t, got.EndingAt)
	assert.GreaterOrEqual(t, got.Disconnected, 29, "the 30 s window was spent away")
	activeMS := got.EndingAt.Sub(*got.StartedAt).Milliseconds() - int64(got.Disconnected)*1000
	assert.Equal(t, int((activeMS+999)/1000), got.Charged, "disconnected_seconds are taken off the bill")
	assert.Less(t, got.Charged, 15, "only the time on the line is charged, not the 30 s away")
	assert.Equal(t, "RECONNECT_TIMEOUT", str(got.EndReason))

	require.Eventually(t, func() bool { _, pub := outboxCount(t, s.ID); return pub == 1 }, 20*time.Second, 200*time.Millisecond)
	total, _ := outboxCount(t, s.ID)
	require.Equal(t, 1, total)
	for _, q := range []string{events.QueueEvaluation, events.QueueMemory, events.QueueSummary} {
		require.Len(t, sessionCompletedMessages(t, rabbitURL, q, s.ID), 1, q)
	}
	var final string
	require.NoError(t, stack.pool.QueryRow(t.Context(),
		`SELECT payload->>'final_status' FROM event_outbox WHERE aggregate_id = $1`, s.ID).Scan(&final))
	assert.Equal(t, "COMPLETED", final)
	assert.Equal(t, 1, len(sessionEvents(t, s.ID)["SESSION_COMPLETED"]))
	testutil.AssertReconciled(t, stack.pool)
}
