//go:build integration

package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	promtest "github.com/prometheus/client_golang/prometheus/testutil"
	amqp091 "github.com/rabbitmq/amqp091-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/amqp"
	"github.com/nabePi/kotoki-backend/internal/events"
	"github.com/nabePi/kotoki-backend/internal/jobs"
	"github.com/nabePi/kotoki-backend/internal/platform/clock"
	"github.com/nabePi/kotoki-backend/internal/postgres"
	"github.com/nabePi/kotoki-backend/internal/redisstore"
	"github.com/nabePi/kotoki-backend/internal/session"
	"github.com/nabePi/kotoki-backend/internal/testutil"
	"github.com/nabePi/kotoki-backend/internal/testutil/sessionenv"
)

// The M6 gate: PG + Redis + RabbitMQ on one fake clock (sessionenv). No consumer is attached unless
// a test says so, so the queues hold what the outbox publisher sent.

var m6Discard = slog.New(slog.NewTextHandler(io.Discard, nil))

func m6PublisherConfig() amqp.PublisherConfig {
	return amqp.PublisherConfig{
		PollInterval: 500 * time.Millisecond, ConfirmTimeout: 5 * time.Second, TxTimeout: 15 * time.Second,
		BatchSize: 100, MaxAttempts: 10,
	}
}

func m6Dial(t *testing.T, url string) *amqp.Conn {
	t.Helper()
	c, err := amqp.Dial(context.Background(), url, "m6-gate", m6Discard)
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// m6Publisher is an outbox publisher on its own broker connection.
func m6Publisher(t *testing.T, e *sessionenv.Env, url string) (*amqp.OutboxPublisher, *amqp.Conn) {
	t.Helper()
	conn := m6Dial(t, url)
	p := amqp.NewOutboxPublisher(e.Pool, amqp.NewConfirmPublisher(conn, 5*time.Second), m6PublisherConfig(),
		amqp.NewMetrics(prometheus.NewRegistry()), m6Discard)
	return p, conn
}

// m6Drain pops every message of queue and returns their message ids (the event ids).
func m6Drain(t *testing.T, url, queue string) []string {
	t.Helper()
	conn, err := amqp091.Dial(url)
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()
	ch, err := conn.Channel()
	require.NoError(t, err)
	var ids []string
	for {
		d, ok, err := ch.Get(queue, true)
		require.NoError(t, err)
		if !ok {
			return ids
		}
		ids = append(ids, d.MessageId)
	}
}

var m6SessionQueues = []string{events.QueueEvaluation, events.QueueMemory, events.QueueSummary}

// m6Sweeper registers the real session_sweeper job on a Scheduler with the real advisory locker;
// e.Sweeper is reused across runs so its heartbeat guard survives between them, as in a long-lived
// worker.
func m6Sweeper(t *testing.T, e *sessionenv.Env, m *jobs.Metrics, log *slog.Logger) *jobs.Scheduler {
	t.Helper()
	if log == nil {
		log = m6Discard
	}
	spec, ok := jobs.SpecByName("session_sweeper")
	require.True(t, ok)
	sched := jobs.NewScheduler(jobs.SchedulerDeps{Locker: jobs.PgLocker{Pool: e.Pool}, Metrics: m, Clock: e.Clock, Log: log})
	sched.Register(spec, jobs.NewSessionSweeperJob(func() jobs.SessionTicker { return e.Sweeper }, m, log, e.Clock, 2*time.Minute))
	return sched
}

func m6RunSweeper(t *testing.T, sched *jobs.Scheduler) {
	t.Helper()
	ran, err := sched.RunOnce(context.Background(), "session_sweeper")
	require.NoError(t, err)
	require.True(t, ran)
}

func m6Status(t *testing.T, e *sessionenv.Env, id uuid.UUID) (status, endReason string, charged int, endedAt time.Time) {
	t.Helper()
	var reason *string
	var ended *time.Time
	require.NoError(t, e.Pool.QueryRow(context.Background(),
		`SELECT status, end_reason, charged_seconds, ended_at FROM sessions WHERE id = $1`, id).Scan(&status, &reason, &charged, &ended))
	if reason != nil {
		endReason = *reason
	}
	if ended != nil {
		endedAt = ended.UTC()
	}
	return
}

// TestM6_SessionCompleted_FlowsToThreeQueues: a normal M3 lifecycle ends in session.completed on
// the three session queues; the other event types reach only their own queue or the unrouted one.
func TestM6_SessionCompleted_FlowsToThreeQueues(t *testing.T) {
	e := sessionenv.New(t)
	url := testutil.NewRabbitMQ(t)
	adm := testutil.RabbitMQAdmin(t, url)
	ctx := context.Background()
	pub, _ := m6Publisher(t, e, url)

	id := e.SeedActive(t, sessionenv.ActiveOpts{DurationSeconds: 300, Beat: true})
	fence := e.Fence(t, id)
	e.Clock.Advance(90 * time.Second)
	_, err := e.Sessions.BeginEnding(ctx, id, &fence, session.EndUserEnded, "", session.ActorRealtime)
	require.NoError(t, err)
	_, err = e.Sessions.Finalize(ctx, id, &fence, session.ActorRealtime)
	require.NoError(t, err)
	status, reason, charged, _ := m6Status(t, e, id)
	require.Equal(t, "COMPLETED", status)
	require.Equal(t, "USER_ENDED", reason)
	require.Equal(t, 90, charged)

	n, err := pub.PublishBatch(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, n)
	for _, q := range m6SessionQueues {
		require.Equal(t, 1, adm.QueueMessages(q), q)
	}
	require.Equal(t, 0, adm.QueueMessages(events.QueueLearner))
	require.Equal(t, 0, adm.QueueMessages(amqp.QueueUnrouted))

	userID := uuid.New()
	evalID := uuid.New()
	now := e.Clock.Now()
	require.NoError(t, e.Store.WithinTx(ctx, func(ctx context.Context, tx *postgres.Tx) error {
		return tx.Outbox().Enqueue(ctx, events.NewEvaluationCompleted(uuid.Must(uuid.NewV7()), now, events.ProducerWorker, userID,
			events.EvaluationCompletedV1{EvaluationID: evalID, SessionID: id, SessionEndedAt: now, CompletedAt: now}))
	}))
	n, err = pub.PublishBatch(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, n)
	require.Equal(t, 1, adm.QueueMessages(events.QueueLearner))
	for _, q := range m6SessionQueues {
		require.Equal(t, 1, adm.QueueMessages(q), "evaluation.completed does not reach "+q)
	}
	require.Equal(t, 0, adm.QueueMessages(amqp.QueueUnrouted))

	require.NoError(t, e.Store.WithinTx(ctx, func(ctx context.Context, tx *postgres.Tx) error {
		return tx.Outbox().Enqueue(ctx, events.NewMemoryExtracted(uuid.Must(uuid.NewV7()), now, events.ProducerWorker, userID,
			events.MemoryExtractedV1{SessionID: id, ExtractedAt: now}))
	}))
	n, err = pub.PublishBatch(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, n)
	require.Equal(t, 1, adm.QueueMessages(amqp.QueueUnrouted), "memory.extracted has no consumer yet and lands in the unrouted queue")
	require.Equal(t, 1, adm.QueueMessages(events.QueueLearner))
	for _, q := range m6SessionQueues {
		require.Equal(t, 1, adm.QueueMessages(q), q)
	}
}

// TestM6_SessionCompleted_ExactlyOnce_RuntimeVsSweeperVsPublisher (review focus 1): per session the
// runtime finalizes with its fence while the sweeper ticks and two publishers poll; the session is
// settled once, one event is written and each queue receives it once.
func TestM6_SessionCompleted_ExactlyOnce_RuntimeVsSweeperVsPublisher(t *testing.T) {
	const sessions = 20
	e := sessionenv.New(t)
	url := testutil.NewRabbitMQ(t)
	ctx := context.Background()

	ids := make([]uuid.UUID, sessions)
	fences := make([]int64, sessions)
	for i := range ids {
		ids[i] = e.SeedActive(t, sessionenv.ActiveOpts{DurationSeconds: 300, Beat: true})
		fences[i] = e.Fence(t, ids[i])
	}
	e.Clock.Advance(30 * time.Second)
	for i, id := range ids {
		_, err := e.Sessions.BeginEnding(ctx, id, &fences[i], session.EndUserEnded, "", session.ActorRealtime)
		require.NoError(t, err)
	}
	e.Clock.Advance(61 * time.Second) // ENDING for over a minute: rule (c) applies

	var pubs []*amqp.OutboxPublisher
	for range 2 {
		p, _ := m6Publisher(t, e, url)
		pubs = append(pubs, p)
	}
	stop := make(chan struct{})
	var pwg sync.WaitGroup
	for _, p := range pubs {
		pwg.Add(1)
		go func() {
			defer pwg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if _, err := p.PublishBatch(ctx); err != nil {
					t.Errorf("publish: %v", err)
					return
				}
				time.Sleep(5 * time.Millisecond)
			}
		}()
	}

	var wg sync.WaitGroup
	for i, id := range ids {
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, err := e.Sessions.Finalize(ctx, id, &fences[i], session.ActorRealtime)
			if err != nil && !errors.Is(err, session.ErrFenceLost) {
				t.Errorf("runtime finalize %s: %v", id, err)
			}
		}()
		go func() {
			defer wg.Done()
			if _, err := e.Sweeper.Tick(ctx); err != nil {
				t.Errorf("sweeper tick: %v", err)
			}
		}()
	}
	wg.Wait()
	close(stop)
	pwg.Wait()
	for {
		n, err := pubs[0].PublishBatch(ctx)
		require.NoError(t, err)
		if n == 0 {
			break
		}
	}

	for _, id := range ids {
		status, _, _, _ := m6Status(t, e, id)
		require.Contains(t, []string{"COMPLETED", "FAILED"}, status)
		require.Equal(t, 1, e.Count(t, `SELECT count(*) FROM event_outbox WHERE aggregate_id = $1 AND event_type = 'session.completed'`, id))
		require.Equal(t, 1, e.Count(t, `SELECT count(*) FROM balance_ledger WHERE session_id = $1 AND entry_type = 'CONSUME'`, id))
		require.Equal(t, 1, e.Count(t, `SELECT count(*) FROM balance_ledger WHERE session_id = $1 AND entry_type = 'RELEASE'`, id))
	}
	require.Equal(t, sessions, e.Count(t, `SELECT count(*) FROM event_outbox WHERE event_type = 'session.completed' AND status = 'PUBLISHED'`))
	for _, q := range m6SessionQueues {
		got := m6Drain(t, url, q)
		require.Len(t, got, sessions, q)
		uniq := map[string]bool{}
		for _, id := range got {
			uniq[id] = true
		}
		require.Len(t, uniq, sessions, "%s: every event_id once", q)
	}
}

// TestM6_PodKillEquivalent_HeartbeatLoss_FailsOnceWithFairCharge (review focus 1): the runtime pod
// dies, nobody writes the session's heartbeat any more. The first sweeper tick only starts the Redis
// guard; once Redis has been healthy for 45 s rule (b) fails the session at its last heartbeat.
func TestM6_PodKillEquivalent_HeartbeatLoss_FailsOnceWithFairCharge(t *testing.T) {
	for _, tc := range []struct {
		name        string
		lastBeat    time.Duration
		wantCharged int
	}{
		{"fair_charge_after_two_minutes", 120 * time.Second, 90}, // D-036: 120 s active, minus 30 s for a service fault
		{"under_a_minute_is_free", 50 * time.Second, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := sessionenv.New(t)
			url := testutil.NewRabbitMQ(t)
			adm := testutil.RabbitMQAdmin(t, url)
			ctx := context.Background()
			m := jobs.NewMetrics(prometheus.NewRegistry())
			sched := m6Sweeper(t, e, m, nil)
			pub, _ := m6Publisher(t, e, url)

			t0 := e.Clock.Now()
			id := e.SeedActive(t, sessionenv.ActiveOpts{DurationSeconds: 900})
			last := t0.Add(tc.lastBeat)
			require.NoError(t, e.Redis.Set(ctx, redisstore.HeartbeatKey(id), strconv.FormatInt(last.UnixMilli(), 10), 0).Err())

			e.Clock.Set(t0.Add(130 * time.Second))
			m6RunSweeper(t, sched)
			status, _, _, _ := m6Status(t, e, id)
			require.Equal(t, "ACTIVE", status, "the guard has not trusted Redis for 45 s yet")
			require.Equal(t, 1.0, promtest.ToFloat64(m.HeartbeatChecksSuspended))

			e.Clock.Set(t0.Add(200 * time.Second))
			m6RunSweeper(t, sched)
			status, reason, charged, endedAt := m6Status(t, e, id)
			require.Equal(t, "FAILED", status)
			require.Equal(t, "SERVER_ERROR", reason)
			require.Equal(t, last, endedAt, "ended at the last heartbeat")
			require.Equal(t, tc.wantCharged, charged)
			require.Equal(t, 1, e.Count(t, `SELECT count(*) FROM event_outbox WHERE aggregate_id = $1 AND event_type = 'session.completed'`, id))
			var final string
			require.NoError(t, e.Pool.QueryRow(ctx, `SELECT payload->>'final_status' FROM event_outbox WHERE aggregate_id = $1`, id).Scan(&final))
			require.Equal(t, "FAILED", final)

			n, err := pub.PublishBatch(ctx)
			require.NoError(t, err)
			require.Equal(t, 1, n)
			for _, q := range m6SessionQueues {
				require.Equal(t, 1, adm.QueueMessages(q), q)
			}

			e.Clock.Set(t0.Add(210 * time.Second))
			m6RunSweeper(t, sched)
			require.Equal(t, 1, e.Count(t, `SELECT count(*) FROM event_outbox WHERE aggregate_id = $1`, id), "the third run is a no-op")
			n, err = pub.PublishBatch(ctx)
			require.NoError(t, err)
			require.Zero(t, n)
			require.Equal(t, 0, e.Count(t, `SELECT count(*) FROM balance_reservations WHERE session_id = $1 AND status = 'HELD'`, id), "R4: no HELD reservation behind a terminal session")
			wantConsume := 0
			if tc.wantCharged > 0 {
				wantConsume = 1
			}
			require.Equal(t, wantConsume, e.Count(t, `SELECT count(*) FROM balance_ledger WHERE session_id = $1 AND entry_type = 'CONSUME'`, id))
		})
	}
}

// m6LogLines is a slog sink that keeps the records for assertions.
type m6LogLines struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *m6LogLines) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *m6LogLines) records(t *testing.T) []map[string]any {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []map[string]any
	for _, line := range bytes.Split(l.buf.Bytes(), []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var rec map[string]any
		require.NoError(t, json.Unmarshal(line, &rec))
		out = append(out, rec)
	}
	return out
}

// TestM6_RedisFlush_DoesNotFailHealthySessions (review focus 2): a FLUSHALL erases every heartbeat;
// the sweeper must recognise the mass loss, fail nothing and say so.
func TestM6_RedisFlush_DoesNotFailHealthySessions(t *testing.T) {
	e := sessionenv.New(t)
	ctx := context.Background()
	m := jobs.NewMetrics(prometheus.NewRegistry())
	sink := &m6LogLines{}
	sched := m6Sweeper(t, e, m, slog.New(slog.NewJSONHandler(sink, nil)))

	var all []uuid.UUID
	for range 25 {
		all = append(all, e.SeedActive(t, sessionenv.ActiveOpts{DurationSeconds: 900, Beat: true}))
	}
	beatAll := func() {
		for _, id := range all {
			e.Beat(t, id)
		}
	}
	e.Clock.Advance(time.Minute)
	beatAll()
	m6RunSweeper(t, sched) // the guard sees Redis for the first time

	e.Clock.Advance(50 * time.Second)
	require.NoError(t, e.Redis.FlushAll(ctx).Err())
	for range 3 { // up to 60 s after the flush
		m6RunSweeper(t, sched)
		e.Clock.Advance(10 * time.Second)
	}

	require.Equal(t, 25, e.Count(t, `SELECT count(*) FROM sessions WHERE status = 'ACTIVE'`), "no healthy session was failed")
	require.Equal(t, 0, e.Count(t, `SELECT count(*) FROM event_outbox`))
	require.GreaterOrEqual(t, promtest.ToFloat64(m.MassHeartbeatLoss), 1.0)
	require.Equal(t, 0.0, promtest.ToFloat64(m.SweeperActions.WithLabelValues("b", "ok")))
	var logged bool
	for _, rec := range sink.records(t) {
		if rec["msg"] == "sweeper.mass_heartbeat_loss" {
			require.Equal(t, "ERROR", rec["level"])
			logged = true
		}
	}
	require.True(t, logged, "ERROR log sweeper.mass_heartbeat_loss")

	beatAll() // the owners rewrite their heartbeats
	m6RunSweeper(t, sched)
	require.Equal(t, 25, e.Count(t, `SELECT count(*) FROM sessions WHERE status = 'ACTIVE'`))
	require.Equal(t, 0.0, promtest.ToFloat64(m.SweeperActions.WithLabelValues("b", "ok")))
}

// TestM6_HandoffSessions_ExcludedFromMassLoss: sessions in handoff have no owner to beat for them
// and must neither be failed by rule (b) nor count towards the mass-loss ratio; rule (e) closes
// them after 30 s with the handoff time as the end of billing.
func TestM6_HandoffSessions_ExcludedFromMassLoss(t *testing.T) {
	e := sessionenv.New(t)
	ctx := context.Background()
	m := jobs.NewMetrics(prometheus.NewRegistry())
	sched := m6Sweeper(t, e, m, nil)
	t0 := e.Clock.Now()

	var handoff, lost []uuid.UUID
	for range 30 {
		handoff = append(handoff, e.SeedActive(t, sessionenv.ActiveOpts{DurationSeconds: 900}))
	}
	for range 5 {
		lost = append(lost, e.SeedActive(t, sessionenv.ActiveOpts{DurationSeconds: 900, Beat: true}))
	}
	m6RunSweeper(t, sched) // t0: the guard starts

	e.Clock.Set(t0.Add(50 * time.Second))
	for _, id := range handoff {
		_, err := e.Sessions.MarkHandoff(ctx, id, e.Fence(t, id))
		require.NoError(t, err)
	}
	e.Clock.Set(t0.Add(60 * time.Second))
	m6RunSweeper(t, sched)

	require.Equal(t, 5.0, promtest.ToFloat64(m.SweeperActions.WithLabelValues("b", "ok")), "only the five dead owners")
	require.Equal(t, 0.0, promtest.ToFloat64(m.MassHeartbeatLoss))
	for _, id := range lost {
		status, reason, _, _ := m6Status(t, e, id)
		require.Equal(t, "FAILED", status)
		require.Equal(t, "SERVER_ERROR", reason)
	}
	require.Equal(t, 30, e.Count(t, `SELECT count(*) FROM sessions WHERE status = 'ACTIVE' AND handoff_at IS NOT NULL`), "handoff sessions untouched by (b)")

	e.Clock.Set(t0.Add(100 * time.Second)) // 50 s after the handoff
	m6RunSweeper(t, sched)
	require.Equal(t, 30.0, promtest.ToFloat64(m.SweeperActions.WithLabelValues("e", "ok")))
	for _, id := range handoff {
		status, reason, charged, endedAt := m6Status(t, e, id)
		require.Equal(t, "COMPLETED", status)
		require.Equal(t, "RECONNECT_TIMEOUT", reason)
		require.Equal(t, 50, charged, "billing ends at handoff_at")
		require.Equal(t, t0.Add(100*time.Second), endedAt)
		var ending time.Time
		require.NoError(t, e.Pool.QueryRow(ctx, `SELECT ending_at FROM sessions WHERE id = $1`, id).Scan(&ending))
		require.Equal(t, t0.Add(50*time.Second), ending.UTC())
	}
}

// m6EffectHandler is a handler shaped as D-093 requires: one transaction that marks the event
// processed and writes the effect. fail, while set, makes it fail permanently before any write.
type m6EffectHandler struct {
	queue string
	store *postgres.Store
	mu    sync.Mutex
	fixed bool
	calls int
}

func (h *m6EffectHandler) Queue() string { return h.queue }
func (h *m6EffectHandler) SupportedEvents() map[string]int {
	return map[string]int{events.SessionCompleted: 1}
}
func (h *m6EffectHandler) Timeout() time.Duration                                  { return 10 * time.Second }
func (h *m6EffectHandler) OnFailure(context.Context, events.Delivery, error, bool) {}
func (h *m6EffectHandler) Handle(ctx context.Context, d events.Delivery) error {
	h.mu.Lock()
	h.calls++
	fixed := h.fixed
	h.mu.Unlock()
	if !fixed {
		return events.Permanent("CONFIGURATION_ERROR", errors.New("prompt missing"))
	}
	return h.store.WithinTx(ctx, func(ctx context.Context, tx *postgres.Tx) error {
		if err := tx.Processed().MarkProcessed(ctx, h.queue, d.Envelope); err != nil {
			return err
		}
		_, err := tx.DBTX().Exec(ctx, `INSERT INTO m6_gate_effects (event_id) VALUES ($1)`, d.Envelope.EventID)
		return err
	})
}

type m6NeverProcessed struct{}

func (m6NeverProcessed) IsProcessed(context.Context, string, uuid.UUID) (bool, error) {
	return false, nil
}

// TestM6_DLQReplay_Works: a real session.completed fails permanently in the consumer and lands in
// the DLQ; after the "fix" ReplayDLQ puts it back and it is processed exactly once.
func TestM6_DLQReplay_Works(t *testing.T) {
	e := sessionenv.New(t)
	url := testutil.NewRabbitMQ(t)
	adm := testutil.RabbitMQAdmin(t, url)
	ctx := context.Background()
	_, err := e.Pool.Exec(ctx, `CREATE TABLE m6_gate_effects (id bigserial PRIMARY KEY, event_id uuid NOT NULL)`)
	require.NoError(t, err)
	pub, _ := m6Publisher(t, e, url)

	id := e.SeedActive(t, sessionenv.ActiveOpts{DurationSeconds: 300, Beat: true})
	fence := e.Fence(t, id)
	e.Clock.Advance(40 * time.Second)
	_, err = e.Sessions.BeginEnding(ctx, id, &fence, session.EndUserEnded, "", session.ActorRealtime)
	require.NoError(t, err)
	_, err = e.Sessions.Finalize(ctx, id, &fence, session.ActorRealtime)
	require.NoError(t, err)
	_, err = pub.PublishBatch(ctx)
	require.NoError(t, err)
	// the evaluation queue is the one under test; the others keep their copy

	h := &m6EffectHandler{queue: events.QueueEvaluation, store: e.Store}
	cm := amqp.NewMetrics(prometheus.NewRegistry())
	consumer := amqp.NewConsumer(h, m6Dial(t, url), amqp.NewConfirmPublisher(m6Dial(t, url), 5*time.Second), m6NeverProcessed{}, cm, m6Discard, clock.System{})
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- consumer.Run(runCtx) }()
	t.Cleanup(func() {
		_ = consumer.Shutdown(ctx, 0)
		cancel()
		select {
		case <-done:
		case <-time.After(15 * time.Second):
		}
	})

	dlq := amqp.DLQName(events.QueueEvaluation)
	require.Eventually(t, func() bool { return adm.QueueMessages(dlq) == 1 }, 15*time.Second, 50*time.Millisecond, "permanent failure goes straight to the DLQ")
	require.Equal(t, 0, adm.QueueMessages(events.QueueEvaluation))
	require.Equal(t, 0, e.Count(t, `SELECT count(*) FROM m6_gate_effects`))

	h.mu.Lock()
	h.fixed = true
	h.mu.Unlock()
	n, err := amqp.ReplayDLQ(ctx, m6Dial(t, url), amqp.NewConfirmPublisher(m6Dial(t, url), 5*time.Second),
		amqp.ReplayOptions{Queue: events.QueueEvaluation, Limit: 10})
	require.NoError(t, err)
	require.Equal(t, 1, n)
	require.Eventually(t, func() bool { return e.Count(t, `SELECT count(*) FROM m6_gate_effects`) == 1 }, 15*time.Second, 50*time.Millisecond)
	require.Eventually(t, func() bool { return adm.QueueMessages(dlq) == 0 && adm.QueueMessages(events.QueueEvaluation) == 0 }, 10*time.Second, 50*time.Millisecond)
	time.Sleep(500 * time.Millisecond)
	require.Equal(t, 1, e.Count(t, `SELECT count(*) FROM m6_gate_effects`), "processed once")
	require.Equal(t, 1, e.Count(t, `SELECT count(*) FROM processed_events WHERE consumer = $1`, events.QueueEvaluation))
	// the sibling queues were never touched by the replay
	require.Equal(t, 1, adm.QueueMessages(events.QueueMemory))
	require.Equal(t, 1, adm.QueueMessages(events.QueueSummary))
}

// TestM6_OutboxSurvivesBrokerOutage: with the broker's connections cut, publish attempts fail
// without burning attempts; after the reconnect the event goes out exactly once.
func TestM6_OutboxSurvivesBrokerOutage(t *testing.T) {
	e := sessionenv.New(t)
	url := testutil.NewRabbitMQ(t)
	adm := testutil.RabbitMQAdmin(t, url)
	ctx := context.Background()
	pub, conn := m6Publisher(t, e, url)

	id := e.SeedActive(t, sessionenv.ActiveOpts{DurationSeconds: 300, Beat: true})
	fence := e.Fence(t, id)
	e.Clock.Advance(45 * time.Second)
	_, err := e.Sessions.BeginEnding(ctx, id, &fence, session.EndUserEnded, "", session.ActorRealtime)
	require.NoError(t, err)
	_, err = e.Sessions.Finalize(ctx, id, &fence, session.ActorRealtime)
	require.NoError(t, err)

	adm.CloseAllConnections()
	require.Eventually(t, func() bool { return !conn.Healthy() }, 10*time.Second, 20*time.Millisecond)
	for range 3 {
		n, err := pub.PublishBatch(ctx)
		require.ErrorIs(t, err, amqp.ErrBrokerUnavailable)
		require.Zero(t, n)
	}
	require.Equal(t, 0, e.Count(t, `SELECT attempts FROM event_outbox WHERE aggregate_id = $1`, id), "a dead broker burns no attempts")
	require.Equal(t, 1, e.Count(t, `SELECT count(*) FROM event_outbox WHERE aggregate_id = $1 AND status = 'PENDING'`, id))

	require.NoError(t, conn.Reconnect(ctx))
	n, err := pub.PublishBatch(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, n)
	n, err = pub.PublishBatch(ctx)
	require.NoError(t, err)
	require.Zero(t, n)
	require.Equal(t, 1, e.Count(t, `SELECT count(*) FROM event_outbox WHERE aggregate_id = $1 AND status = 'PUBLISHED'`, id))
	for _, q := range m6SessionQueues {
		require.Equal(t, 1, adm.QueueMessages(q), q)
	}
	assert.Equal(t, 0, e.Count(t, `SELECT attempts FROM event_outbox WHERE aggregate_id = $1`, id))
}
