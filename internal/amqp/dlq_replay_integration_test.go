//go:build integration

package amqp_test

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	amqp091 "github.com/rabbitmq/amqp091-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/amqp"
	"github.com/nabePi/kotoki-backend/internal/events"
	"github.com/nabePi/kotoki-backend/internal/testutil"
)

// deadHeaders are the headers a dead-lettered message carries (spec 9.7.4).
func deadHeaders(class string) amqp091.Table {
	return amqp091.Table{
		"x-retry-count": int32(4), "x-original-queue": events.QueueEvaluation, "x-last-error": "boom",
		"x-first-failed-at": "2026-10-16T00:00:00Z", "x-error-class": class, "x-dead-at": "2026-10-16T00:10:00Z",
	}
}

// publishToDLQ puts a message on <queue>.dlq through kotoki.dlx and returns its envelope and body.
func publishToDLQ(t *testing.T, url, queue, class string) (events.Envelope, []byte) {
	t.Helper()
	env, body := testEnvelope(t, events.SessionCompleted)
	pub := amqp.NewConfirmPublisher(dial(t, url), 5*time.Second)
	require.NoError(t, pub.PublishConfirmed(context.Background(), amqp.ExchangeDLX, queue, amqp091.Publishing{
		Headers: deadHeaders(class), ContentType: "application/json", DeliveryMode: amqp091.Persistent,
		MessageId: env.EventID.String(), Type: env.EventType, AppId: "kotoki", Timestamp: env.OccurredAt, Body: body,
	}))
	return env, body
}

func replayPublisher(t *testing.T, url string) (*amqp.Conn, *amqp.ConfirmPublisher) {
	t.Helper()
	c := dial(t, url)
	return c, amqp.NewConfirmPublisher(dial(t, url), 5*time.Second)
}

func TestReplayDLQ_MovesBackToMainQueueWithResetHeaders(t *testing.T) {
	url := testutil.NewRabbitMQ(t)
	adm := testutil.RabbitMQAdmin(t, url)
	const queue = events.QueueEvaluation
	bodies := map[string][]byte{}
	for range 3 {
		env, body := publishToDLQ(t, url, queue, "CONFIGURATION_ERROR")
		bodies[env.EventID.String()] = body
	}
	require.Equal(t, 3, adm.QueueMessages(amqp.DLQName(queue)))
	conn, pub := replayPublisher(t, url)

	n, err := amqp.ReplayDLQ(context.Background(), conn, pub, amqp.ReplayOptions{Queue: queue, Limit: 100})

	require.NoError(t, err)
	assert.Equal(t, 3, n)
	assert.Equal(t, 0, adm.QueueMessages(amqp.DLQName(queue)), "DLQ is empty")
	assert.Equal(t, 3, adm.QueueMessages(queue), "the messages are back on the main queue")
	assert.Equal(t, 0, adm.QueueMessages(events.QueueMemory))
	assert.Equal(t, 0, adm.QueueMessages(events.QueueSummary))
	for range 3 {
		d := getOne(t, url, queue)
		assert.Equal(t, int32(0), d.Headers["x-retry-count"], "x-retry-count reset to int32 0")
		replayedAt, ok := d.Headers["x-replayed-at"].(string)
		require.True(t, ok, "x-replayed-at is set")
		_, perr := time.Parse(time.RFC3339Nano, replayedAt)
		assert.NoError(t, perr)
		assert.Equal(t, "CONFIGURATION_ERROR", d.Headers["x-error-class"])
		assert.Equal(t, "boom", d.Headers["x-last-error"])
		assert.Equal(t, queue, d.Headers["x-original-queue"])
		assert.Equal(t, "2026-10-16T00:00:00Z", d.Headers["x-first-failed-at"])
		assert.Equal(t, "2026-10-16T00:10:00Z", d.Headers["x-dead-at"])
		want, found := bodies[d.MessageId]
		require.True(t, found)
		assert.Equal(t, want, d.Body, "body untouched")
		assert.Equal(t, events.SessionCompleted, d.Type)
		assert.Equal(t, amqp091.Persistent, d.DeliveryMode)
	}
}

func TestReplayDLQ_Limit(t *testing.T) {
	url := testutil.NewRabbitMQ(t)
	adm := testutil.RabbitMQAdmin(t, url)
	const queue = events.QueueEvaluation
	for range 5 {
		publishToDLQ(t, url, queue, "ENTITY_NOT_FOUND")
	}
	conn, pub := replayPublisher(t, url)

	n, err := amqp.ReplayDLQ(context.Background(), conn, pub, amqp.ReplayOptions{Queue: queue, Limit: 2})

	require.NoError(t, err)
	assert.Equal(t, 2, n)
	assert.Equal(t, 3, adm.QueueMessages(amqp.DLQName(queue)))
	assert.Equal(t, 2, adm.QueueMessages(queue))
}

func TestReplayDLQ_DryRun_PrintsAndLeavesAll(t *testing.T) {
	url := testutil.NewRabbitMQ(t)
	adm := testutil.RabbitMQAdmin(t, url)
	const queue = events.QueueEvaluation
	var ids []string
	for range 3 {
		env, _ := publishToDLQ(t, url, queue, "INVARIANT_VIOLATION")
		ids = append(ids, env.EventID.String())
	}
	conn, pub := replayPublisher(t, url)
	var out bytes.Buffer

	n, err := amqp.ReplayDLQ(context.Background(), conn, pub, amqp.ReplayOptions{Queue: queue, Limit: 100, DryRun: true, Out: &out})

	require.NoError(t, err)
	assert.Equal(t, 3, n, "dry run counts the messages it listed")
	for _, id := range ids {
		assert.Contains(t, out.String(), id)
	}
	assert.Equal(t, 3, bytes.Count(out.Bytes(), []byte("INVARIANT_VIOLATION")), "one x-error-class per line")
	assert.Equal(t, 3, bytes.Count(out.Bytes(), []byte("boom")), "x-last-error printed")
	assert.Equal(t, 3, bytes.Count(out.Bytes(), []byte(events.SessionCompleted)), "event_type printed")
	require.Eventually(t, func() bool { return adm.QueueMessages(amqp.DLQName(queue)) == 3 }, 5*time.Second, 50*time.Millisecond,
		"every message is back in the DLQ after the single multiple nack")
	assert.Equal(t, 0, adm.QueueMessages(queue))
}

func TestReplayDLQ_DryRun_LimitListsOnlyLimit(t *testing.T) {
	url := testutil.NewRabbitMQ(t)
	adm := testutil.RabbitMQAdmin(t, url)
	const queue = events.QueueMemory
	for range 4 {
		publishToDLQ(t, url, queue, "ENTITY_NOT_FOUND")
	}
	conn, pub := replayPublisher(t, url)
	var out bytes.Buffer

	n, err := amqp.ReplayDLQ(context.Background(), conn, pub, amqp.ReplayOptions{Queue: queue, Limit: 2, DryRun: true, Out: &out})

	require.NoError(t, err)
	assert.Equal(t, 2, n)
	assert.Equal(t, 2, bytes.Count(out.Bytes(), []byte("\n")))
	require.Eventually(t, func() bool { return adm.QueueMessages(amqp.DLQName(queue)) == 4 }, 5*time.Second, 50*time.Millisecond)
}

func TestReplayDLQ_UnknownQueue_ErrorBeforeBroker(t *testing.T) {
	// a nil Conn would panic if the broker were touched
	n, err := amqp.ReplayDLQ(context.Background(), nil, nil, amqp.ReplayOptions{Queue: "nope.queue", Limit: 1})

	require.Error(t, err)
	assert.Equal(t, 0, n)
	assert.Contains(t, err.Error(), "nope.queue")
}

type failingPublisher struct{ err error }

func (f failingPublisher) PublishConfirmed(context.Context, string, string, amqp091.Publishing) error {
	return f.err
}

func TestReplayDLQ_PublishFailure_KeepsMessageInDLQ(t *testing.T) {
	url := testutil.NewRabbitMQ(t)
	adm := testutil.RabbitMQAdmin(t, url)
	const queue = events.QueueEvaluation
	publishToDLQ(t, url, queue, "CONFIGURATION_ERROR")
	conn, _ := replayPublisher(t, url)

	n, err := amqp.ReplayDLQ(context.Background(), conn, failingPublisher{errors.New("broker said no")}, amqp.ReplayOptions{Queue: queue, Limit: 10})

	require.Error(t, err)
	assert.Equal(t, 0, n)
	require.Eventually(t, func() bool { return adm.QueueMessages(amqp.DLQName(queue)) == 1 }, 5*time.Second, 50*time.Millisecond,
		"a message whose replay publish failed is not lost")
}

func TestReplayDLQ_EndToEnd_PoisonThenFixedHandler(t *testing.T) {
	url := testutil.NewRabbitMQ(t)
	adm := testutil.RabbitMQAdmin(t, url)
	const queue = events.QueueEvaluation
	var fixed bool
	h := &queueHandler{queue: queue}
	h.fn = func(context.Context, events.Delivery) error {
		h.mu.Lock()
		ok := fixed
		h.mu.Unlock()
		if !ok {
			return events.Permanent("CONFIGURATION_ERROR", errors.New("missing prompt"))
		}
		return nil
	}
	rc := startConsumer(t, url, h, fakeProcessed{})
	env, body := testEnvelope(t, events.SessionCompleted)
	publishToQueue(t, url, queue, env, body, nil)
	require.Eventually(t, func() bool { return adm.QueueMessages(amqp.DLQName(queue)) == 1 }, 10*time.Second, 50*time.Millisecond)

	h.mu.Lock()
	fixed = true
	h.mu.Unlock()
	conn, pub := replayPublisher(t, url)
	n, err := amqp.ReplayDLQ(context.Background(), conn, pub, amqp.ReplayOptions{Queue: queue, Limit: 10})

	require.NoError(t, err)
	assert.Equal(t, 1, n)
	require.Eventually(t, func() bool { return resultCount(rc, queue, "ack") == 1 }, 10*time.Second, 50*time.Millisecond,
		"the replayed message is processed by the fixed handler")
	assert.Equal(t, 0, adm.QueueMessages(amqp.DLQName(queue)))
	assert.Equal(t, 0, adm.QueueMessages(queue))
	ds := h.deliveries()
	require.Len(t, ds, 2)
	assert.Equal(t, 0, ds[1].RetryCount, "the replayed delivery starts the ladder from zero")
	assert.Equal(t, env.EventID, ds[1].Envelope.EventID)
}
