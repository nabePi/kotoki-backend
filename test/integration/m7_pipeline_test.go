//go:build integration

package integration_test

import (
	"context"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	amqp091 "github.com/rabbitmq/amqp091-go"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/amqp"
	"github.com/nabePi/kotoki-backend/internal/evaluation"
	"github.com/nabePi/kotoki-backend/internal/events"
	"github.com/nabePi/kotoki-backend/internal/platform/clock"
	"github.com/nabePi/kotoki-backend/internal/platform/idgen"
	"github.com/nabePi/kotoki-backend/internal/postgres"
	"github.com/nabePi/kotoki-backend/internal/summary"
	"github.com/nabePi/kotoki-backend/internal/testutil"
)

// The M7 pipeline: session.completed written to the outbox in a transaction -> outbox publisher ->
// RabbitMQ -> the real evaluation and summary consumers (amqp.Consumer around the feature handlers,
// fake providers) -> evaluations / sessions.summary_text -> evaluation.completed or .failed back
// through the outbox. Postgres and RabbitMQ are real containers.

const (
	evalConsumerName    = events.QueueEvaluation
	summaryConsumerName = events.QueueSummary
)

type pipeline struct {
	pool *pgxpool.Pool
	url  string
	adm  *testutil.RabbitAdmin
	rt   *textRouter
}

// startPipeline runs the outbox publisher and both consumers until the test ends.
func startPipeline(t *testing.T, pool *pgxpool.Pool, url string) *pipeline {
	t.Helper()
	log := slog.New(slog.DiscardHandler)
	store := postgres.NewStore(pool)
	p := &pipeline{pool: pool, url: url, adm: testutil.RabbitMQAdmin(t, url), rt: newTextRouter()}

	am := amqp.NewMetrics(prometheus.NewRegistry())
	pubConn := m6Dial(t, url)
	confirm := amqp.NewConfirmPublisher(pubConn, 5*time.Second)
	outbox := amqp.NewOutboxPublisher(pool, confirm, amqp.PublisherConfig{
		PollInterval: 100 * time.Millisecond, ConfirmTimeout: 5 * time.Second, TxTimeout: 15 * time.Second, BatchSize: 100, MaxAttempts: 10,
	}, am, log)

	evalCfg := evaluation.DefaultConfig()
	evalSvc := evaluation.NewService(evaluation.Deps{
		Store: store.EvaluationStore(), Source: postgres.NewEvaluationSource(pool), Content: postgres.NewContentRepo(pool),
		Router: withFake(), Cfg: evalCfg, Clock: clock.System{}, IDs: idgen.NewV7(), Schemas: evaluation.NewSchemaCache(), Log: log,
	})
	sumSvc := summary.NewService(summary.Deps{
		Store: store.SummaryStore(), Source: postgres.NewSummarySource(pool), Prompts: postgres.NewSummaryPromptSource(pool),
		Router: p.rt, Cfg: summary.DefaultConfig(), Log: log,
	})
	processed := postgres.NewProcessedEventsReader(pool)
	consumers := []*amqp.Consumer{
		amqp.NewConsumer(evaluation.NewConsumer(evalSvc, evalCfg), m6Dial(t, url), confirm, processed, am, log, clock.System{}),
		amqp.NewConsumer(summary.NewConsumer(sumSvc), m6Dial(t, url), confirm, processed, am, log, clock.System{}),
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{}, 1+len(consumers))
	go func() { _ = outbox.Run(ctx); done <- struct{}{} }()
	for _, c := range consumers {
		go func() { _ = c.Run(ctx); done <- struct{}{} }()
	}
	t.Cleanup(func() {
		for _, c := range consumers {
			_ = c.Shutdown(context.Background(), 0)
		}
		cancel()
		for range 1 + len(consumers) {
			select {
			case <-done:
			case <-time.After(15 * time.Second):
				t.Error("pipeline goroutine did not stop")
				return
			}
		}
	})
	return p
}

// announce writes the session.completed event of fs to the outbox, as the session service does.
func announce(t *testing.T, pool *pgxpool.Pool, fs testutil.FinishedSession) {
	t.Helper()
	store := postgres.NewStore(pool)
	ev := events.NewSessionCompleted(fs.Envelope.EventID, fs.Payload.EndedAt, events.ProducerRealtime, fs.UserID, fs.Payload)
	require.NoError(t, store.WithinTx(context.Background(), func(ctx context.Context, tx *postgres.Tx) error {
		return tx.Outbox().Enqueue(ctx, ev)
	}))
}

func (p *pipeline) evaluationStatus(t *testing.T, sessionID uuid.UUID) string {
	t.Helper()
	var st string
	err := p.pool.QueryRow(context.Background(), `SELECT status FROM evaluations WHERE session_id = $1`, sessionID).Scan(&st)
	if err != nil {
		return ""
	}
	return st
}

func (p *pipeline) summaryText(t *testing.T, sessionID uuid.UUID) *string {
	t.Helper()
	return loadSummary(t, p.pool, sessionID).Text
}

// deadLetter pops the one message of queue and returns its x-error-class header.
func deadLetter(t *testing.T, url, queue string) string {
	t.Helper()
	conn, err := amqp091.Dial(url)
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()
	ch, err := conn.Channel()
	require.NoError(t, err)
	d, ok, err := ch.Get(queue, true)
	require.NoError(t, err)
	require.True(t, ok, queue)
	class, _ := d.Headers["x-error-class"].(string)
	return class
}

func (p *pipeline) requireQueuesDrained(t *testing.T) {
	t.Helper()
	for _, q := range []string{events.QueueEvaluation, events.QueueSummary, amqp.DLQName(events.QueueEvaluation), amqp.DLQName(events.QueueSummary)} {
		require.Eventually(t, func() bool { return p.adm.QueueMessages(q) == 0 }, 15*time.Second, 100*time.Millisecond, q)
	}
}

func TestPipeline_SessionCompletedToEvaluationAndSummary(t *testing.T) {
	pool := seededPool(t)
	url := testutil.NewRabbitMQ(t)
	p := startPipeline(t, pool, url)
	fs := testutil.SeedFinishedSession(t, pool, testutil.FinishedSessionOpts{Turns: seedTurns(5, 25), ActiveSeconds: 120})
	announce(t, pool, fs)

	require.Eventually(t, func() bool {
		return p.evaluationStatus(t, fs.ID) == "COMPLETED" && p.summaryText(t, fs.ID) != nil &&
			outboxPublished(t, pool, "evaluation.completed", fs.ID) == 1
	}, 30*time.Second, 100*time.Millisecond, "evaluation COMPLETED, summary written and evaluation.completed published")

	require.Positive(t, count(t, pool, `SELECT count(*) FROM evaluation_findings f JOIN evaluations e ON e.id = f.evaluation_id WHERE e.session_id = $1`, fs.ID),
		"findings from the fake provider")
	require.Equal(t, 1, count(t, pool, `SELECT count(*) FROM evaluations WHERE session_id = $1`, fs.ID))
	require.Equal(t, 1, outboxCount(t, pool, "evaluation.completed", fs.ID))
	require.Zero(t, outboxCount(t, pool, "evaluation.failed", fs.ID))
	require.NotEmpty(t, *p.summaryText(t, fs.ID))
	require.Equal(t, 1, count(t, pool, `SELECT count(*) FROM processed_events WHERE consumer = $1 AND event_id = $2`, evalConsumerName, fs.Envelope.EventID))
	require.Equal(t, 1, count(t, pool, `SELECT count(*) FROM processed_events WHERE consumer = $1 AND event_id = $2`, summaryConsumerName, fs.Envelope.EventID))
	require.Equal(t, 1, count(t, pool, `SELECT count(*) FROM event_outbox WHERE event_type = 'session.completed' AND status = 'PUBLISHED'`))

	// evaluation.completed is routed to the learner queue (no consumer yet in M7)
	require.Eventually(t, func() bool { return p.adm.QueueMessages(events.QueueLearner) == 1 }, 15*time.Second, 100*time.Millisecond)
	p.requireQueuesDrained(t)
	require.Zero(t, p.adm.QueueMessages(amqp.QueueUnrouted))
}

func outboxPublished(t *testing.T, pool *pgxpool.Pool, typ string, sessionID uuid.UUID) int {
	t.Helper()
	return count(t, pool, `SELECT count(*) FROM event_outbox o WHERE o.event_type = $1 AND o.status = 'PUBLISHED' AND o.payload->>'session_id' = $2`, typ, sessionID.String())
}

func TestPipeline_EvaluationAPIThreeStates(t *testing.T) {
	l := newLifecycle(t)
	url := testutil.NewRabbitMQ(t)
	userA, authzA := l.newUser()
	_, authzB := l.newUser()
	get := func(authz string, id uuid.UUID) lcResp {
		return l.call(http.MethodGet, "/api/v1/sessions/"+id.String()+"/evaluation", "", "Authorization", authz)
	}

	// CANCELLED (never started) -> 404 EVALUATION_NOT_AVAILABLE
	created := l.create(authzA, "pipeline-cancel-0001", "AI_FIRST", "600")
	require.Equal(t, 201, created.code, string(created.body))
	cancelID := l.sessionID(created)
	require.Equal(t, 200, l.call(http.MethodPost, "/api/v1/sessions/"+cancelID.String()+"/cancel", "", "Authorization", authzA).code)
	r := get(authzA, cancelID)
	require.Equal(t, http.StatusNotFound, r.code, string(r.body))
	require.Equal(t, "EVALUATION_NOT_AVAILABLE", r.errCode(t))

	// ACTIVE -> 409 SESSION_NOT_ENDED with the status
	created = l.create(authzA, "pipeline-active-0001", "AI_FIRST", "600")
	require.Equal(t, 201, created.code, string(created.body))
	activeID := l.sessionID(created)
	l.start(activeID)
	r = get(authzA, activeID)
	require.Equal(t, http.StatusConflict, r.code, string(r.body))
	require.Equal(t, "SESSION_NOT_ENDED", r.errCode(t))
	details := r.json(t)["error"].(map[string]any)["details"].(map[string]any)
	require.Equal(t, "ACTIVE", details["session_status"])

	// COMPLETED with no evaluation row yet (the consumers are not running) -> 200 PENDING + Retry-After
	fs := testutil.SeedFinishedSession(t, l.pool, testutil.FinishedSessionOpts{UserID: userA, Turns: seedTurns(5, 25), ActiveSeconds: 120})
	r = get(authzA, fs.ID)
	require.Equal(t, http.StatusOK, r.code, string(r.body))
	require.Equal(t, "PENDING", r.json(t)["status"])
	require.Equal(t, "3", r.header.Get("Retry-After"))

	// the pipeline runs -> 200 COMPLETED
	startPipeline(t, l.pool, url)
	announce(t, l.pool, fs)
	require.Eventually(t, func() bool {
		r := get(authzA, fs.ID)
		return r.code == http.StatusOK && r.json(t)["status"] == "COMPLETED"
	}, 30*time.Second, 200*time.Millisecond, "evaluation COMPLETED through the API")
	r = get(authzA, fs.ID)
	body := r.json(t)
	require.Equal(t, "rubric-v1", body["rubric_version"])
	require.Empty(t, r.header.Get("Retry-After"))
	require.NotEmpty(t, body["findings"], "findings from the fake provider")

	// another user's session is indistinguishable from a missing one
	r = get(authzB, fs.ID)
	require.Equal(t, http.StatusNotFound, r.code, string(r.body))
	require.Equal(t, "SESSION_NOT_FOUND", r.errCode(t))
	r = get(authzB, activeID)
	require.Equal(t, "SESSION_NOT_FOUND", r.errCode(t))
}

func TestPipeline_PermanentErrorGoesToDLQ(t *testing.T) {
	pool := seededPool(t)
	url := testutil.NewRabbitMQ(t)
	p := startPipeline(t, pool, url)
	// the event announces 3 user turns, the database holds none: TRANSCRIPT_UNAVAILABLE
	fs := testutil.SeedFinishedSession(t, pool, testutil.FinishedSessionOpts{ActiveSeconds: 120}).
		WithPayload(t, func(sp *events.SessionCompletedV1) { sp.UserTurnCount = 3 })
	announce(t, pool, fs)

	evalDLQ := amqp.DLQName(events.QueueEvaluation)
	require.Eventually(t, func() bool { return p.adm.QueueMessages(evalDLQ) == 1 }, 30*time.Second, 100*time.Millisecond,
		"permanent failure goes straight to the evaluation DLQ")
	require.Equal(t, "TRANSCRIPT_UNAVAILABLE", deadLetter(t, url, evalDLQ))
	require.Eventually(t, func() bool { return p.evaluationStatus(t, fs.ID) == "FAILED" }, 15*time.Second, 100*time.Millisecond)
	require.Eventually(t, func() bool { return outboxPublished(t, pool, "evaluation.failed", fs.ID) == 1 }, 15*time.Second, 100*time.Millisecond)
	require.Eventually(t, func() bool { return p.adm.QueueMessages(amqp.QueueUnrouted) == 1 }, 15*time.Second, 100*time.Millisecond,
		"evaluation.failed has no consumer and lands in the unrouted queue")
	require.Zero(t, outboxCount(t, pool, "evaluation.completed", fs.ID))
	require.Zero(t, count(t, pool, `SELECT count(*) FROM processed_events WHERE consumer = $1 AND event_id = $2`, evalConsumerName, fs.Envelope.EventID),
		"a final FAILED writes no processed_events row")
	require.Nil(t, p.summaryText(t, fs.ID))
	require.Eventually(t, func() bool { return p.adm.QueueMessages(events.QueueEvaluation) == 0 }, 15*time.Second, 100*time.Millisecond)
}
