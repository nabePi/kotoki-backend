//go:build integration

package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	amqp091 "github.com/rabbitmq/amqp091-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/amqp"
	"github.com/nabePi/kotoki-backend/internal/events"
	"github.com/nabePi/kotoki-backend/internal/platform/clock"
	"github.com/nabePi/kotoki-backend/internal/testutil"
)

// workerEnv is a complete worker environment on migrated Postgres, Redis and RabbitMQ containers.
func workerEnv(t *testing.T, overrides map[string]string) (env map[string]string, pool *pgxpool.Pool, rabbitURL string) {
	t.Helper()
	pool = testutil.NewPostgres(t)
	rabbitURL = testutil.NewRabbitMQ(t)
	m := map[string]string{
		"DB_URL": pool.Config().ConnString(), "REDIS_URL": "redis://" + testutil.NewRedis(t).Options().Addr,
		"RABBITMQ_URL": rabbitURL, "WORKER_HTTP_ADDR": testutil.FreeAddr(t), "LOG_FORMAT": "text",
		"WORKER_SHUTDOWN_GRACE": "5s", "SWEEPER_INTERVAL": "300ms",
	}
	for k, v := range overrides {
		m[k] = v
	}
	env = testutil.MinimalEnv(t, m)
	testutil.ApplyEnv(t, env)
	return env, pool, rabbitURL
}

func status(addr, path string) (int, string) {
	resp, err := http.Get("http://" + addr + path)
	if err != nil {
		return 0, ""
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestWorker_RunCore_ReadyAndMetrics(t *testing.T) {
	env, _, url := workerEnv(t, nil)
	addr := env["WORKER_HTTP_ADDR"]
	adm := testutil.RabbitMQAdmin(t, url)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- runWith(ctx, []string{"--handlers=outbox,sweeper,cleanup"}, io.Discard, io.Discard)
	}()

	require.Eventually(t, func() bool { c, _ := status(addr, "/readyz"); return c == 200 }, 10*time.Second, 50*time.Millisecond,
		"/readyz 200 within 10 s")
	require.Eventually(t, func() bool {
		_, m := status(addr, "/metrics")
		return strings.Contains(m, "kotoki_outbox_pending") && strings.Contains(m, "kotoki_job_runs_total{")
	}, 10*time.Second, 50*time.Millisecond, "outbox gauge and a counted job run on /metrics")

	// Dynamic readiness: a dead broker connection turns /readyz 503, the keep-alive brings it back.
	var saw503 atomic.Bool
	stop := make(chan struct{})
	sampled := make(chan struct{})
	go func() {
		defer close(sampled)
		for {
			select {
			case <-stop:
				return
			default:
			}
			if c, _ := status(addr, "/readyz"); c == 503 {
				saw503.Store(true)
			}
		}
	}()
	adm.CloseAllConnections()
	require.Eventually(t, saw503.Load, 10*time.Second, 5*time.Millisecond, "/readyz went 503 when the connection died")
	require.Eventually(t, func() bool { c, _ := status(addr, "/readyz"); return c == 200 }, 30*time.Second, 100*time.Millisecond,
		"/readyz 200 again after the reconnect")
	close(stop)
	<-sampled

	start := time.Now()
	cancel()
	select {
	case err := <-done:
		require.NoError(t, err)
		assert.Less(t, time.Since(start), 5*time.Second+shutdownSlack, "stops within WORKER_SHUTDOWN_GRACE")
	case <-time.After(15 * time.Second):
		t.Fatal("run did not return after cancel")
	}
}

func TestWorker_RunJob_ExecutesWithAdvisoryLock(t *testing.T) {
	_, pool, _ := workerEnv(t, nil)
	runJob := func() string {
		var out bytes.Buffer
		require.NoError(t, runWith(context.Background(), []string{"run-job", "outbox_retention"}, &out, io.Discard))
		return strings.TrimSpace(out.String())
	}
	assert.Equal(t, "job=outbox_retention ran=true", runJob())

	conn, err := pool.Acquire(context.Background())
	require.NoError(t, err)
	defer conn.Release()
	_, err = conn.Exec(context.Background(), `SELECT pg_advisory_lock(7100005)`)
	require.NoError(t, err)
	assert.Equal(t, "job=outbox_retention ran=false", runJob(), "the lock is held elsewhere: exit 0, ran=false")
	_, err = conn.Exec(context.Background(), `SELECT pg_advisory_unlock(7100005)`)
	require.NoError(t, err)
	assert.Equal(t, "job=outbox_retention ran=true", runJob())
}

func TestWorker_RunJob_EveryJobOfTheTableRuns(t *testing.T) {
	workerEnv(t, nil)
	for _, name := range jobNames() {
		var out bytes.Buffer
		require.NoError(t, runWith(context.Background(), []string{"run-job", name}, &out, io.Discard), name)
		assert.Equal(t, "job="+name+" ran=true\n", out.String())
	}
}

func publishDead(t *testing.T, url, queue string) {
	t.Helper()
	conn, err := amqp.Dial(context.Background(), url, "test", slog.New(slog.DiscardHandler))
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	id := uuid.Must(uuid.NewV7())
	require.NoError(t, amqp.NewConfirmPublisher(conn, 5*time.Second).PublishConfirmed(context.Background(), amqp.ExchangeDLX, queue,
		amqp091.Publishing{
			Headers:     amqp091.Table{"x-retry-count": int32(4), "x-error-class": "CONFIGURATION_ERROR", "x-last-error": "boom"},
			ContentType: "application/json", DeliveryMode: amqp091.Persistent, MessageId: id.String(),
			Type: events.SessionCompleted, AppId: "kotoki", Body: []byte(`{"event_id":"` + id.String() + `"}`),
		}))
}

func TestWorker_DLQReplay_Subcommand(t *testing.T) {
	_, _, url := workerEnv(t, nil)
	adm := testutil.RabbitMQAdmin(t, url)
	const queue = events.QueueEvaluation
	publishDead(t, url, queue)
	publishDead(t, url, queue)
	require.Equal(t, 2, adm.QueueMessages(amqp.DLQName(queue)))

	replay := func(args ...string) string {
		var out bytes.Buffer
		require.NoError(t, runWith(context.Background(), append([]string{"dlq-replay"}, args...), &out, io.Discard))
		return out.String()
	}

	out := replay("--queue", queue, "--dry-run")
	assert.Contains(t, out, "x-error-class=CONFIGURATION_ERROR")
	assert.Contains(t, out, "queue="+queue+" dry_run=true listed=2")
	assert.Equal(t, 2, adm.QueueMessages(amqp.DLQName(queue)), "dry run leaves everything")

	assert.Equal(t, "queue="+queue+" replayed=1\n", replay("--queue", queue, "--limit", "1"))
	assert.Equal(t, 1, adm.QueueMessages(amqp.DLQName(queue)))
	assert.Equal(t, 1, adm.QueueMessages(queue))

	assert.Equal(t, "queue="+queue+" replayed=1\n", replay("--queue", queue))
	assert.Equal(t, 0, adm.QueueMessages(amqp.DLQName(queue)))
	assert.Equal(t, 2, adm.QueueMessages(queue))
}

// blockingHandler holds every delivery until its context is cancelled.
type blockingHandler struct {
	started   chan struct{}
	cancelled atomic.Bool
	failures  atomic.Int32
}

func (h *blockingHandler) Queue() string { return events.QueueEvaluation }
func (h *blockingHandler) SupportedEvents() map[string]int {
	return map[string]int{events.SessionCompleted: 1}
}
func (h *blockingHandler) Timeout() time.Duration { return time.Minute }
func (h *blockingHandler) Handle(ctx context.Context, _ events.Delivery) error {
	select {
	case h.started <- struct{}{}:
	default:
	}
	<-ctx.Done()
	h.cancelled.Store(true)
	return ctx.Err()
}
func (h *blockingHandler) OnFailure(context.Context, events.Delivery, error, bool) { h.failures.Add(1) }

type neverProcessed struct{}

func (neverProcessed) IsProcessed(context.Context, string, uuid.UUID) (bool, error) {
	return false, nil
}

// The shutdown order of cmd/worker for consumers: stop taking deliveries, give the handler the grace,
// cancel it, put the message back (no retry, no OnFailure), and only then return.
func TestWorker_StopConsumers_GraceThenCancelThenRequeue(t *testing.T) {
	url := testutil.NewRabbitMQ(t)
	adm := testutil.RabbitMQAdmin(t, url)
	log := slog.New(slog.DiscardHandler)
	conn, err := amqp.Dial(context.Background(), url, "consumer", log)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	pconn, err := amqp.Dial(context.Background(), url, "pub", log)
	require.NoError(t, err)
	t.Cleanup(func() { _ = pconn.Close() })
	pub := amqp.NewConfirmPublisher(pconn, 5*time.Second)

	h := &blockingHandler{started: make(chan struct{}, 1)}
	c := amqp.NewConsumer(h, conn, pub, neverProcessed{}, amqp.NewMetrics(prometheus.NewRegistry()), log, clock.System{})
	runCtx, stopRun := context.WithCancel(context.Background())
	defer stopRun()
	var runErr error
	var mu sync.Mutex
	done := goDone(func() { e := c.Run(runCtx); mu.Lock(); runErr = e; mu.Unlock() })

	env, body := func() (events.Envelope, []byte) {
		id := uuid.Must(uuid.NewV7())
		b := []byte(`{"event_id":"` + id.String() + `","event_type":"session.completed","event_version":1,` +
			`"occurred_at":"2026-10-16T00:00:00Z","producer":"REALTIME","aggregate_type":"session",` +
			`"aggregate_id":"` + uuid.NewString() + `","user_id":"` + uuid.NewString() + `","payload":{}}`)
		return events.Envelope{EventID: id, EventType: events.SessionCompleted}, b
	}()
	require.NoError(t, pub.PublishConfirmed(context.Background(), amqp.ExchangeRequeue, events.QueueEvaluation, amqp091.Publishing{
		ContentType: "application/json", DeliveryMode: amqp091.Persistent, MessageId: env.EventID.String(),
		Type: events.SessionCompleted, AppId: "kotoki", Body: body,
	}))
	select {
	case <-h.started:
	case <-time.After(10 * time.Second):
		t.Fatal("handler never started")
	}

	const grace = 700 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), grace+shutdownSlack)
	defer cancel()
	start := time.Now()
	require.NoError(t, stopConsumers(ctx, grace, []*amqp.Consumer{c}, []<-chan struct{}{done}, stopRun))
	took := time.Since(start)

	assert.GreaterOrEqual(t, took, grace, "the handler got the whole grace before it was cancelled")
	assert.Less(t, took, grace+shutdownSlack)
	assert.True(t, h.cancelled.Load(), "the root context was cancelled after the grace")
	assert.Zero(t, h.failures.Load(), "no OnFailure on shutdown requeue")
	mu.Lock()
	assert.NoError(t, runErr)
	mu.Unlock()
	assert.Eventually(t, func() bool { return adm.QueueMessages(events.QueueEvaluation) == 1 }, 5*time.Second, 100*time.Millisecond,
		"the message is back on its queue")
	assert.Zero(t, adm.QueueMessages(amqp.DLQName(events.QueueEvaluation)))
}
