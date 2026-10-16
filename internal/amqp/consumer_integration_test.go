//go:build integration

package amqp_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	amqp091 "github.com/rabbitmq/amqp091-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/trace"

	"github.com/nabePi/kotoki-backend/internal/amqp"
	"github.com/nabePi/kotoki-backend/internal/events"
	"github.com/nabePi/kotoki-backend/internal/postgres"
	"github.com/nabePi/kotoki-backend/internal/testutil"
)

// neverProcessed is a pre-check that always says "not yet", so MarkProcessed is the only guard.
type neverProcessed struct{}

func (neverProcessed) IsProcessed(context.Context, string, uuid.UUID) (bool, error) {
	return false, nil
}

// effectHandler is the shape every real handler must have (D-093): one transaction that calls
// MarkProcessed first and then writes the business effect, here a row in m6_effects.
func effectHandler(t *testing.T, pool *pgxpool.Pool, queue string, after func(ctx context.Context, d events.Delivery)) *queueHandler {
	t.Helper()
	_, err := pool.Exec(context.Background(), `CREATE TABLE IF NOT EXISTS m6_effects (id bigserial PRIMARY KEY, event_id uuid NOT NULL)`)
	require.NoError(t, err)
	store := postgres.NewStore(pool)
	return &queueHandler{queue: queue, fn: func(ctx context.Context, d events.Delivery) error {
		err := store.WithinTx(ctx, func(ctx context.Context, tx *postgres.Tx) error {
			if err := tx.Processed().MarkProcessed(ctx, queue, d.Envelope); err != nil {
				return err
			}
			_, err := tx.DBTX().Exec(ctx, `INSERT INTO m6_effects (event_id) VALUES ($1)`, d.Envelope.EventID)
			return err
		})
		if err == nil && after != nil {
			after(ctx, d)
		}
		return err
	}}
}

func count(t *testing.T, pool *pgxpool.Pool, query string, args ...any) int {
	t.Helper()
	var n int
	require.NoError(t, pool.QueryRow(context.Background(), query, args...).Scan(&n))
	return n
}

func resultCount(rc *runningConsumer, queue, result string) float64 {
	return counter(rc.m.ConsumerMessages.WithLabelValues(queue, result))
}

func TestRetry_FirstRung_RealBroker(t *testing.T) {
	url := testutil.NewRabbitMQ(t)
	adm := testutil.RabbitMQAdmin(t, url)
	const queue = events.QueueEvaluation
	var mu sync.Mutex
	var at []time.Time
	h := &queueHandler{queue: queue, fn: func(context.Context, events.Delivery) error {
		mu.Lock()
		defer mu.Unlock()
		at = append(at, time.Now())
		if len(at) == 1 {
			return errors.New("transient boom")
		}
		return nil
	}}
	rc := startConsumer(t, url, h, fakeProcessed{})
	env, body := testEnvelope(t, events.SessionCompleted)

	publishToQueue(t, url, queue, env, body, nil)

	require.Eventually(t, func() bool { return adm.QueueMessages(amqp.RetryQueueName(queue, amqp.RetryLevels[0])) == 1 },
		3*time.Second, 25*time.Millisecond, "the failed message waits in the 5s retry queue")
	require.Eventually(t, func() bool { return h.handledCount() == 2 }, 15*time.Second, 50*time.Millisecond)
	mu.Lock()
	gap := at[1].Sub(at[0])
	mu.Unlock()
	assert.GreaterOrEqual(t, gap, 4*time.Second, "not before the 5s TTL (>= 4s)")
	assert.LessOrEqual(t, gap, 9*time.Second)
	ds := h.deliveries()
	assert.Equal(t, 0, ds[0].RetryCount)
	assert.Equal(t, 1, ds[1].RetryCount, "x-retry-count=1 on the retried delivery")
	assert.Equal(t, env.EventID, ds[1].Envelope.EventID)
	require.Eventually(t, func() bool { return resultCount(rc, queue, "ack") == 1 }, 5*time.Second, 25*time.Millisecond)
	assert.EqualValues(t, 1, resultCount(rc, queue, "retry"))
	calls := h.failureCalls()
	require.Len(t, calls, 1)
	assert.False(t, calls[0].final, "OnFailure(final=false) before the retry")
	assert.EqualError(t, calls[0].cause, "transient boom")
	assert.Equal(t, 0, adm.QueueMessages(queue))
	assert.Equal(t, 0, adm.QueueMessages(amqp.DLQName(queue)))
}

func TestPoison_PermanentError_LandsInDLQWithHeaders(t *testing.T) {
	url := testutil.NewRabbitMQ(t)
	adm := testutil.RabbitMQAdmin(t, url)
	const queue = events.QueueEvaluation
	poison, poisonBody := testEnvelope(t, events.SessionCompleted)
	h := &queueHandler{queue: queue, fn: func(_ context.Context, d events.Delivery) error {
		if d.Envelope.EventID == poison.EventID {
			return events.Permanent("ENTITY_NOT_FOUND", errors.New("session gone"))
		}
		return nil
	}}
	rc := startConsumer(t, url, h, fakeProcessed{})
	good, goodBody := testEnvelope(t, events.SessionCompleted)

	publishToQueue(t, url, queue, poison, poisonBody, nil)
	publishToQueue(t, url, queue, good, goodBody, nil)

	require.Eventually(t, func() bool { return adm.QueueMessages(amqp.DLQName(queue)) == 1 }, 10*time.Second, 50*time.Millisecond)
	require.Eventually(t, func() bool { return resultCount(rc, queue, "ack") == 1 }, 10*time.Second, 50*time.Millisecond,
		"the queue is not blocked: the next message is processed")
	assert.Equal(t, 0, adm.QueueMessages(queue))
	for _, lvl := range amqp.RetryLevels {
		assert.Equal(t, 0, adm.QueueMessages(amqp.RetryQueueName(queue, lvl)), "permanent errors skip the ladder")
	}
	d := getOne(t, url, amqp.DLQName(queue))
	assert.Equal(t, poison.EventID.String(), d.MessageId)
	assert.Equal(t, "ENTITY_NOT_FOUND", d.Headers["x-error-class"])
	assert.Equal(t, queue, d.Headers["x-original-queue"])
	assert.Contains(t, d.Headers["x-last-error"], "session gone")
	assert.NotEmpty(t, d.Headers["x-dead-at"])
	assert.Equal(t, poisonBody, d.Body, "the body is never modified")
	assert.EqualValues(t, 1, resultCount(rc, queue, "dead"))
	assert.Equal(t, 2, h.handledCount(), "poison handled once, good handled once")
	calls := h.failureCalls()
	require.Len(t, calls, 1)
	assert.True(t, calls[0].final)
}

func TestPoison_MalformedBody_DLQDecodeError(t *testing.T) {
	url := testutil.NewRabbitMQ(t)
	adm := testutil.RabbitMQAdmin(t, url)
	const queue = events.QueueLearner
	h := &queueHandler{queue: queue}
	rc := startConsumer(t, url, h, fakeProcessed{})
	env, _ := testEnvelope(t, events.EvaluationCompleted)

	publishToQueue(t, url, queue, env, []byte(`{"event_id": not json`), nil)

	require.Eventually(t, func() bool { return adm.QueueMessages(amqp.DLQName(queue)) == 1 }, 10*time.Second, 50*time.Millisecond)
	d := getOne(t, url, amqp.DLQName(queue))
	assert.Equal(t, "DECODE_ERROR", d.Headers["x-error-class"])
	assert.Equal(t, queue, d.Headers["x-original-queue"])
	assert.Equal(t, 0, h.handledCount(), "an undecodable body never reaches the handler")
	assert.Equal(t, 0, adm.QueueMessages(queue))
	assert.EqualValues(t, 1, resultCount(rc, queue, "dead"))
}

// TestPoison_DeliveryLimit_DeadLettersToDLQ: a message whose consumer keeps dying (here: the channel
// closes with the delivery unacked, as when a pod crashes) is dead-lettered by the queue itself
// after x-delivery-limit=10 returns, i.e. on the 11th delivery, so a crash loop cannot block the queue.
func TestPoison_DeliveryLimit_DeadLettersToDLQ(t *testing.T) {
	url := testutil.NewRabbitMQ(t)
	adm := testutil.RabbitMQAdmin(t, url)
	const queue = events.QueueEvaluation
	_ = dial(t, url) // declares the topology
	env, body := testEnvelope(t, events.SessionCompleted)
	publishToQueue(t, url, queue, env, body, nil)

	conn, err := amqp091.Dial(url)
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()

	deliveries := 0
	for adm.QueueMessages(amqp.DLQName(queue)) == 0 {
		require.LessOrEqual(t, deliveries, 12, "x-delivery-limit=10 must stop the redelivery loop")
		ch, err := conn.Channel()
		require.NoError(t, err)
		_, ok, err := ch.Get(queue, false)
		require.NoError(t, err)
		if ok {
			deliveries++
		} else {
			time.Sleep(20 * time.Millisecond) // the broker is still returning the previous delivery
		}
		require.NoError(t, ch.Close())
	}

	assert.Equal(t, 11, deliveries, "delivered 1 + limit 10 times, then dead-lettered by the queue's own DLX")
	assert.Equal(t, 1, adm.QueueMessages(amqp.DLQName(queue)))
	assert.Equal(t, 0, adm.QueueMessages(queue))
	d := getOne(t, url, amqp.DLQName(queue))
	assert.Equal(t, env.EventID.String(), d.MessageId)
	assert.Equal(t, body, d.Body)
}

// TestDeliveryLimit_NackRequeueDoesNotCount pins a RabbitMQ 4 behaviour the consumer design must
// respect: basic.nack(requeue=true) does not advance the delivery count, so only crashes and
// channel closes are bounded by x-delivery-limit. Shutdown requeue therefore never dead-letters.
func TestDeliveryLimit_NackRequeueDoesNotCount(t *testing.T) {
	url := testutil.NewRabbitMQ(t)
	adm := testutil.RabbitMQAdmin(t, url)
	const queue = events.QueueEvaluation
	_ = dial(t, url)
	env, body := testEnvelope(t, events.SessionCompleted)
	publishToQueue(t, url, queue, env, body, nil)

	conn, err := amqp091.Dial(url)
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()
	ch, err := conn.Channel()
	require.NoError(t, err)
	require.NoError(t, ch.Qos(1, 0, false))
	ds, err := ch.Consume(queue, "", false, false, false, false, nil)
	require.NoError(t, err)

	for i := range 13 {
		select {
		case d := <-ds:
			assert.NotContains(t, d.Headers, "x-delivery-count", "delivery %d", i+1)
			require.NoError(t, d.Nack(false, true))
		case <-time.After(5 * time.Second):
			t.Fatalf("delivery %d never came: the message left the queue", i+1)
		}
	}
	assert.Equal(t, 0, adm.QueueMessages(amqp.DLQName(queue)), "13 nack-requeues, still not dead-lettered")
}

func TestIdempotency_DuplicateDelivery_SingleEffect(t *testing.T) {
	pool := testutil.NewPostgres(t)
	url := testutil.NewRabbitMQ(t)
	const queue = events.QueueEvaluation
	h := effectHandler(t, pool, queue, nil)
	rc := startConsumer(t, url, h, postgres.NewProcessedEventsReader(pool))
	env, body := testEnvelope(t, events.SessionCompleted)

	publishToQueue(t, url, queue, env, body, nil)
	require.Eventually(t, func() bool { return resultCount(rc, queue, "ack") == 1 }, 10*time.Second, 25*time.Millisecond)
	publishToQueue(t, url, queue, env, body, nil)

	require.Eventually(t, func() bool { return resultCount(rc, queue, "duplicate") == 1 }, 10*time.Second, 25*time.Millisecond)
	assert.Equal(t, 1, count(t, pool, `SELECT count(*) FROM m6_effects WHERE event_id = $1`, env.EventID))
	assert.Equal(t, 1, count(t, pool, `SELECT count(*) FROM processed_events WHERE consumer = $1 AND event_id = $2`, queue, env.EventID))
	assert.Equal(t, 1, h.handledCount(), "the second delivery is acked by the pre-check without running the handler")
}

func TestIdempotency_PrecheckBypassed_MarkProcessedStillGuards(t *testing.T) {
	pool := testutil.NewPostgres(t)
	url := testutil.NewRabbitMQ(t)
	const queue = events.QueueEvaluation
	env, body := testEnvelope(t, events.SessionCompleted)
	// both copies are queued before the consumer starts, so they are handled concurrently
	publishToQueue(t, url, queue, env, body, nil)
	publishToQueue(t, url, queue, env, body, nil)
	h := effectHandler(t, pool, queue, nil)
	rc := startConsumer(t, url, h, neverProcessed{})

	require.Eventually(t, func() bool { return resultCount(rc, queue, "ack") == 2 }, 10*time.Second, 25*time.Millisecond)
	assert.Equal(t, 2, h.handledCount(), "no pre-check: both deliveries reach the handler")
	assert.Equal(t, 1, count(t, pool, `SELECT count(*) FROM m6_effects WHERE event_id = $1`, env.EventID), "still exactly one effect")
	assert.Equal(t, 1, count(t, pool, `SELECT count(*) FROM processed_events WHERE event_id = $1`, env.EventID))
	assert.EqualValues(t, 0, resultCount(rc, queue, "retry")+resultCount(rc, queue, "dead"))
}

func TestIdempotency_ForcedRedelivery_AfterCommitBeforeAck(t *testing.T) {
	pool := testutil.NewPostgres(t)
	url := testutil.NewRabbitMQ(t)
	adm := testutil.RabbitMQAdmin(t, url)
	const queue = events.QueueEvaluation
	committed := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	h := effectHandler(t, pool, queue, func(ctx context.Context, _ events.Delivery) {
		once.Do(func() { close(committed) })
		select { // the effect is committed; the ack has not been sent yet
		case <-release:
		case <-ctx.Done():
		}
	})
	rc := startConsumer(t, url, h, postgres.NewProcessedEventsReader(pool))
	env, body := testEnvelope(t, events.SessionCompleted)
	publishToQueue(t, url, queue, env, body, nil)
	<-committed
	require.Equal(t, 1, count(t, pool, `SELECT count(*) FROM m6_effects WHERE event_id = $1`, env.EventID))

	adm.CloseAllConnections() // the consumer's connection dies with the delivery unacked
	close(release)            // the handler returns nil; its Ack fails on the dead channel

	require.Eventually(t, func() bool { return resultCount(rc, queue, "duplicate") == 1 }, 20*time.Second, 50*time.Millisecond,
		"after reconnect the broker redelivers and the pre-check recognises the committed event")
	assert.Equal(t, 1, h.handledCount(), "the handler did not run again")
	assert.Equal(t, 1, count(t, pool, `SELECT count(*) FROM m6_effects WHERE event_id = $1`, env.EventID), "one effect despite the redelivery")
	assert.Equal(t, 1, count(t, pool, `SELECT count(*) FROM processed_events WHERE event_id = $1`, env.EventID))
	require.Eventually(t, func() bool { return adm.QueueMessages(queue) == 0 }, 5*time.Second, 50*time.Millisecond)
}

func TestTrace_OutboxToConsumer_SameTraceID(t *testing.T) {
	testutil.NewTraceRecorder(t)
	pool := testutil.NewPostgres(t)
	url := testutil.NewRabbitMQ(t)
	store := postgres.NewStore(pool)
	ev := sessionCompleted(uuid.New())

	got := make(chan trace.SpanContext, 1)
	h := &queueHandler{queue: events.QueueEvaluation, fn: func(ctx context.Context, _ events.Delivery) error {
		got <- trace.SpanContextFromContext(ctx)
		return nil
	}}
	startConsumer(t, url, h, postgres.NewProcessedEventsReader(pool))

	ctx, span := tracerForTest().Start(context.Background(), "business")
	require.NoError(t, store.WithinTx(ctx, func(ctx context.Context, tx *postgres.Tx) error {
		return tx.Outbox().Enqueue(ctx, ev)
	}))
	span.End()
	p, _ := newPublisher(pool, amqp.NewConfirmPublisher(dial(t, url), 5*time.Second), cfg())
	n, err := p.PublishBatch(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, n)

	select {
	case sc := <-got:
		require.True(t, sc.IsValid())
		assert.Equal(t, span.SpanContext().TraceID(), sc.TraceID(), "outbox -> consumer stays in the trace of the business transaction")
		assert.NotEqual(t, span.SpanContext().SpanID(), sc.SpanID(), "the handler runs in its own consumer span")
	case <-time.After(10 * time.Second):
		t.Fatal("the handler was never called")
	}
}
