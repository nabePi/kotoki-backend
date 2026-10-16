//go:build integration

package amqp_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	amqp091 "github.com/rabbitmq/amqp091-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/amqp"
	"github.com/nabePi/kotoki-backend/internal/events"
	"github.com/nabePi/kotoki-backend/internal/platform/clock"
	"github.com/nabePi/kotoki-backend/internal/testutil"
)

// ---- shared helpers of the consumer integration suites ---------------------------------------

// queueHandler is a Handler bound to a real queue of the topology.
type queueHandler struct {
	queue    string
	supports map[string]int
	timeout  time.Duration
	fn       func(ctx context.Context, d events.Delivery) error

	mu       sync.Mutex
	handled  []events.Delivery
	failures []failureCall
}

func (h *queueHandler) Queue() string { return h.queue }
func (h *queueHandler) SupportedEvents() map[string]int {
	if h.supports != nil {
		return h.supports
	}
	return map[string]int{events.SessionCompleted: 1, events.EvaluationCompleted: 1}
}
func (h *queueHandler) Timeout() time.Duration {
	if h.timeout > 0 {
		return h.timeout
	}
	return 10 * time.Second
}
func (h *queueHandler) Handle(ctx context.Context, d events.Delivery) error {
	h.mu.Lock()
	h.handled = append(h.handled, d)
	h.mu.Unlock()
	if h.fn == nil {
		return nil
	}
	return h.fn(ctx, d)
}
func (h *queueHandler) OnFailure(_ context.Context, _ events.Delivery, cause error, final bool) {
	h.mu.Lock()
	h.failures = append(h.failures, failureCall{cause, final})
	h.mu.Unlock()
}
func (h *queueHandler) handledCount() int { h.mu.Lock(); defer h.mu.Unlock(); return len(h.handled) }
func (h *queueHandler) deliveries() []events.Delivery {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]events.Delivery(nil), h.handled...)
}
func (h *queueHandler) failureCalls() []failureCall {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]failureCall(nil), h.failures...)
}

// runningConsumer is a Consumer running against the real broker on its own connection; its retry
// and dead-letter publishes go through a second connection, as in the worker.
type runningConsumer struct {
	c       *amqp.Consumer
	m       *amqp.Metrics
	conn    *amqp.Conn
	cancel  context.CancelFunc
	done    chan error
	stopped bool
}

func startConsumer(t *testing.T, url string, h amqp.Handler, processed amqp.ProcessedChecker) *runningConsumer {
	t.Helper()
	m := amqp.NewMetrics(prometheus.NewRegistry())
	conn := dial(t, url)
	pub := amqp.NewConfirmPublisher(dial(t, url), 5*time.Second)
	c := amqp.NewConsumer(h, conn, pub, processed, m, discard, clock.System{})
	ctx, cancel := context.WithCancel(context.Background())
	rc := &runningConsumer{c: c, m: m, conn: conn, cancel: cancel, done: make(chan error, 1)}
	go func() { rc.done <- c.Run(ctx) }()
	t.Cleanup(rc.stop)
	return rc
}

// stop shuts the consumer down with a zero grace and waits for Run to return.
func (rc *runningConsumer) stop() {
	if rc.stopped {
		return
	}
	rc.stopped = true
	_ = rc.c.Shutdown(context.Background(), 0)
	rc.cancel()
	select {
	case <-rc.done:
	case <-time.After(15 * time.Second):
	}
}

func testEnvelope(t *testing.T, eventType string) (events.Envelope, []byte) {
	t.Helper()
	env := events.Envelope{
		EventID: uuid.Must(uuid.NewV7()), EventType: eventType, EventVersion: 1,
		OccurredAt: time.Now().UTC(), Producer: events.ProducerRealtime, AggregateType: events.AggregateSession,
		AggregateID: uuid.New(), UserID: uuid.New(), Payload: json.RawMessage(`{"n":1}`),
	}
	b, err := json.Marshal(env)
	require.NoError(t, err)
	return env, b
}

// publishToQueue puts body on queue only (through kotoki.requeue, which binds each main queue by
// its own name) and waits for the broker confirm.
func publishToQueue(t *testing.T, url, queue string, env events.Envelope, body []byte, headers amqp091.Table) {
	t.Helper()
	pub := amqp.NewConfirmPublisher(dial(t, url), 5*time.Second)
	require.NoError(t, pub.PublishConfirmed(context.Background(), amqp.ExchangeRequeue, queue, amqp091.Publishing{
		Headers: headers, ContentType: "application/json", DeliveryMode: amqp091.Persistent,
		MessageId: env.EventID.String(), Type: env.EventType, AppId: "kotoki", Timestamp: env.OccurredAt, Body: body,
	}))
}

// getOne pops one message from queue (basic.get with auto-ack), failing when it is empty.
func getOne(t *testing.T, url, queue string) amqp091.Delivery {
	t.Helper()
	conn, err := amqp091.Dial(url)
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()
	ch, err := conn.Channel()
	require.NoError(t, err)
	d, ok, err := ch.Get(queue, true)
	require.NoError(t, err)
	require.True(t, ok, "queue %s is empty", queue)
	return d
}

// ---- Task 10 ---------------------------------------------------------------------------------

// gate is a handler body that counts concurrency and holds every call until released.
type gate struct {
	release  chan struct{}
	inflight atomic.Int64
	peak     atomic.Int64
}

func newGate() *gate { return &gate{release: make(chan struct{})} }

func (g *gate) handle(ctx context.Context, _ events.Delivery) error {
	n := g.inflight.Add(1)
	for {
		p := g.peak.Load()
		if n <= p || g.peak.CompareAndSwap(p, n) {
			break
		}
	}
	defer g.inflight.Add(-1)
	select {
	case <-g.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestConsumer_PrefetchLimitsConcurrency(t *testing.T) {
	for _, tc := range []struct {
		queue    string
		prefetch int
	}{
		{events.QueueLearner, 16},
		{events.QueueEvaluation, 4},
	} {
		t.Run(tc.queue, func(t *testing.T) {
			url := testutil.NewRabbitMQ(t)
			adm := testutil.RabbitMQAdmin(t, url)
			const total = 40
			for range total {
				env, body := testEnvelope(t, events.SessionCompleted)
				publishToQueue(t, url, tc.queue, env, body, nil)
			}
			g := newGate()
			h := &queueHandler{queue: tc.queue, fn: g.handle}
			rc := startConsumer(t, url, h, fakeProcessed{})

			require.Eventually(t, func() bool { return g.inflight.Load() == int64(tc.prefetch) }, 10*time.Second, 20*time.Millisecond)
			assert.Equal(t, total-tc.prefetch, adm.QueueMessages(tc.queue), "the broker holds back everything beyond the prefetch window")

			close(g.release)
			require.Eventually(t, func() bool { return h.handledCount() == total }, 15*time.Second, 20*time.Millisecond)
			assert.EqualValues(t, tc.prefetch, g.peak.Load(), "peak concurrency == prefetch")
			rc.stop()
		})
	}
}

func TestConsumer_ShutdownWithinGrace_AcksInflight(t *testing.T) {
	url := testutil.NewRabbitMQ(t)
	adm := testutil.RabbitMQAdmin(t, url)
	env, body := testEnvelope(t, events.SessionCompleted)
	publishToQueue(t, url, events.QueueEvaluation, env, body, nil)
	started := make(chan struct{})
	h := &queueHandler{queue: events.QueueEvaluation, fn: func(ctx context.Context, _ events.Delivery) error {
		close(started)
		select {
		case <-time.After(300 * time.Millisecond):
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}}
	rc := startConsumer(t, url, h, fakeProcessed{})
	<-started

	require.NoError(t, rc.c.Shutdown(context.Background(), 2*time.Second))

	require.Eventually(t, func() bool {
		return adm.QueueMessages(events.QueueEvaluation) == 0
	}, 5*time.Second, 50*time.Millisecond)
	assert.Equal(t, 1, h.handledCount())
	assert.Empty(t, h.failureCalls())
	assert.Equal(t, 0, adm.QueueMessages(amqp.DLQName(events.QueueEvaluation)))
	select {
	case err := <-rc.done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after Shutdown")
	}
}

func TestConsumer_ShutdownGraceExpires_RequeuesWithoutRetryCount(t *testing.T) {
	url := testutil.NewRabbitMQ(t)
	adm := testutil.RabbitMQAdmin(t, url)
	env, body := testEnvelope(t, events.SessionCompleted)
	publishToQueue(t, url, events.QueueEvaluation, env, body, nil)
	started := make(chan struct{})
	h := &queueHandler{queue: events.QueueEvaluation, fn: func(ctx context.Context, _ events.Delivery) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	}}
	rc := startConsumer(t, url, h, fakeProcessed{})
	<-started

	require.NoError(t, rc.c.Shutdown(context.Background(), 200*time.Millisecond))

	require.Eventually(t, func() bool {
		return adm.QueueMessages(events.QueueEvaluation) == 1
	}, 5*time.Second, 50*time.Millisecond)
	assert.Empty(t, h.failureCalls(), "OnFailure is not called on shutdown requeue")
	for _, lvl := range amqp.RetryLevels {
		assert.Equal(t, 0, adm.QueueMessages(amqp.RetryQueueName(events.QueueEvaluation, lvl)))
	}
	assert.Equal(t, 0, adm.QueueMessages(amqp.DLQName(events.QueueEvaluation)))
	d := getOne(t, url, events.QueueEvaluation)
	assert.True(t, d.Redelivered)
	assert.NotContains(t, d.Headers, "x-retry-count")
	assert.EqualValues(t, 1, counter(rc.m.ConsumerMessages.WithLabelValues(events.QueueEvaluation, "requeue")))
}

func TestConsumer_UnstartedPrefetchedMessagesRequeued(t *testing.T) {
	url := testutil.NewRabbitMQ(t)
	adm := testutil.RabbitMQAdmin(t, url)
	const total = 10
	for range total {
		env, body := testEnvelope(t, events.SessionCompleted)
		publishToQueue(t, url, events.QueueEvaluation, env, body, nil)
	}
	g := newGate()
	h := &queueHandler{queue: events.QueueEvaluation, fn: g.handle}
	rc := startConsumer(t, url, h, fakeProcessed{})
	require.Eventually(t, func() bool { return g.inflight.Load() == 4 }, 10*time.Second, 20*time.Millisecond)

	require.NoError(t, rc.c.Shutdown(context.Background(), 100*time.Millisecond))

	require.Eventually(t, func() bool {
		return adm.QueueMessages(events.QueueEvaluation) == total
	}, 5*time.Second, 50*time.Millisecond)
	assert.Equal(t, 4, h.handledCount(), "only the four in-flight deliveries were ever started")
	assert.Empty(t, h.failureCalls())
}

func TestConsumer_ReconnectsAfterForcedClose(t *testing.T) {
	url := testutil.NewRabbitMQ(t)
	adm := testutil.RabbitMQAdmin(t, url)
	h := &queueHandler{queue: events.QueueLearner}
	startConsumer(t, url, h, fakeProcessed{})
	env, body := testEnvelope(t, events.EvaluationCompleted)
	publishToQueue(t, url, events.QueueLearner, env, body, nil)
	require.Eventually(t, func() bool { return h.handledCount() == 1 }, 10*time.Second, 20*time.Millisecond)

	adm.CloseAllConnections()

	// the consumer recovers on its own; the consumer takes the message published after the drop
	env2, body2 := testEnvelope(t, events.EvaluationCompleted)
	publishToQueue(t, url, events.QueueLearner, env2, body2, nil)
	require.Eventually(t, func() bool { return h.handledCount() == 2 }, 10*time.Second, 50*time.Millisecond)
	assert.Equal(t, 0, adm.QueueMessages(events.QueueLearner))
}

type brokenPublisher struct{}

func (brokenPublisher) PublishConfirmed(context.Context, string, string, amqp091.Publishing) error {
	return amqp.ErrNacked
}

// TestRetryFallback_BoundedWhileRetryPublisherIsBroken: RabbitMQ 4 does not count a nack requeue
// toward x-delivery-limit, so with a retry publisher that always fails the only brake is the
// fallback delay. A failing handler must be redelivered about once per second, not in a hot loop,
// and the message must survive in the queue.
func TestRetryFallback_BoundedWhileRetryPublisherIsBroken(t *testing.T) {
	url := testutil.NewRabbitMQ(t)
	adm := testutil.RabbitMQAdmin(t, url)
	const queue = events.QueueEvaluation
	h := &queueHandler{queue: queue, fn: func(context.Context, events.Delivery) error { return errors.New("boom") }}
	m := amqp.NewMetrics(prometheus.NewRegistry())
	c := amqp.NewConsumer(h, dial(t, url), brokenPublisher{}, fakeProcessed{}, m, discard, clock.System{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()
	env, body := testEnvelope(t, events.SessionCompleted)
	publishToQueue(t, url, queue, env, body, nil)

	time.Sleep(3500 * time.Millisecond)
	n := h.handledCount()
	assert.GreaterOrEqual(t, n, 2, "the message keeps being redelivered")
	assert.LessOrEqual(t, n, 5, "about one delivery per fallback delay, not a hot loop")
	assert.GreaterOrEqual(t, counter(m.ConsumerMessages.WithLabelValues(queue, "retry_publish_failed")), float64(n-1))

	start := time.Now()
	require.NoError(t, c.Shutdown(context.Background(), 0))
	cancel()
	require.NoError(t, <-done)
	assert.Less(t, time.Since(start), 3*time.Second, "shutdown does not wait for the fallback delay")
	assert.Eventually(t, func() bool { return adm.QueueMessages(queue) == 1 }, 5*time.Second, 100*time.Millisecond,
		"the message is not lost")
	assert.Equal(t, 0, adm.QueueMessages(amqp.DLQName(queue)))
}
