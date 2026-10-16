//go:build integration

package amqp_test

import (
	"context"
	"testing"
	"time"

	amqp091 "github.com/rabbitmq/amqp091-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/amqp"
	"github.com/nabePi/kotoki-backend/internal/events"
	"github.com/nabePi/kotoki-backend/internal/testutil"
)

// topologyEnv declares the topology on a fresh vhost and returns an open channel and the admin.
func topologyEnv(t *testing.T) (*amqp091.Channel, *testutil.RabbitAdmin) {
	t.Helper()
	url := testutil.NewRabbitMQ(t)
	conn, err := amqp091.Dial(url)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	ch, err := conn.Channel()
	require.NoError(t, err)
	require.NoError(t, amqp.DeclareTopology(ch))
	return ch, testutil.RabbitMQAdmin(t, url)
}

func publish(t *testing.T, ch *amqp091.Channel, exchange, key string) {
	t.Helper()
	require.NoError(t, ch.PublishWithContext(context.Background(), exchange, key, false, false,
		amqp091.Publishing{DeliveryMode: amqp091.Persistent, Body: []byte(`{}`)}))
}

func TestDeclareTopology_Idempotent_25Queues(t *testing.T) {
	ch, adm := topologyEnv(t)
	require.NoError(t, amqp.DeclareTopology(ch), "second declare must succeed")

	assert.Len(t, adm.Queues(), 25)
	assert.ElementsMatch(t, []string{
		"kotoki.dlx", "kotoki.events", "kotoki.events.unrouted", "kotoki.requeue", "kotoki.retry",
	}, adm.Exchanges())
}

func TestQueueArguments(t *testing.T) {
	_, adm := topologyEnv(t)
	q := events.QueueEvaluation

	assert.Equal(t, map[string]any{
		"x-queue-type":              "quorum",
		"x-delivery-limit":          float64(10),
		"x-dead-letter-exchange":    "kotoki.dlx",
		"x-dead-letter-routing-key": q,
		"x-dead-letter-strategy":    "at-least-once",
		"x-overflow":                "reject-publish",
	}, adm.QueueArgs(q))

	for _, lvl := range amqp.RetryLevels {
		assert.Equal(t, map[string]any{
			"x-queue-type":              "quorum",
			"x-message-ttl":             float64(lvl.TTL.Milliseconds()),
			"x-dead-letter-exchange":    "kotoki.requeue",
			"x-dead-letter-routing-key": q,
			"x-dead-letter-strategy":    "at-least-once",
			"x-overflow":                "reject-publish",
		}, adm.QueueArgs(amqp.RetryQueueName(q, lvl)), lvl.Suffix)
	}
	assert.Equal(t, map[string]any{"x-queue-type": "quorum"}, adm.QueueArgs(amqp.DLQName(q)))
	assert.Equal(t, map[string]any{
		"x-queue-type":  "quorum",
		"x-message-ttl": float64(259200000),
		"x-max-length":  float64(100000),
		"x-overflow":    "drop-head",
	}, adm.QueueArgs(amqp.QueueUnrouted))
}

func counts(adm *testutil.RabbitAdmin, queues ...string) []int {
	out := make([]int, len(queues))
	for i, q := range queues {
		out[i] = adm.QueueMessages(q)
	}
	return out
}

func TestRouting_SessionCompleted_OneMessageInEachOfThreeQueues(t *testing.T) {
	ch, adm := topologyEnv(t)
	qs := []string{events.QueueEvaluation, events.QueueMemory, events.QueueSummary, events.QueueLearner, amqp.QueueUnrouted}
	publish(t, ch, amqp.ExchangeEvents, "session.completed")

	require.Eventually(t, func() bool { return assert.ObjectsAreEqual([]int{1, 1, 1, 0, 0}, counts(adm, qs...)) },
		5*time.Second, 100*time.Millisecond)
	time.Sleep(500 * time.Millisecond)
	assert.Equal(t, []int{1, 1, 1, 0, 0}, counts(adm, qs...), "exactly one each, nothing elsewhere")
}

func TestRouting_EvaluationCompleted_OnlyLearnerQueue(t *testing.T) {
	ch, adm := topologyEnv(t)
	qs := []string{events.QueueEvaluation, events.QueueMemory, events.QueueSummary, events.QueueLearner, amqp.QueueUnrouted}
	publish(t, ch, amqp.ExchangeEvents, "evaluation.completed")

	require.Eventually(t, func() bool { return assert.ObjectsAreEqual([]int{0, 0, 0, 1, 0}, counts(adm, qs...)) },
		5*time.Second, 100*time.Millisecond)
	time.Sleep(500 * time.Millisecond)
	assert.Equal(t, []int{0, 0, 0, 1, 0}, counts(adm, qs...))
}

func TestRouting_MemoryExtracted_And_EvaluationFailed_GoToUnrouted(t *testing.T) {
	ch, adm := topologyEnv(t)
	qs := []string{events.QueueEvaluation, events.QueueMemory, events.QueueSummary, events.QueueLearner, amqp.QueueUnrouted}
	publish(t, ch, amqp.ExchangeEvents, "memory.extracted")
	publish(t, ch, amqp.ExchangeEvents, "evaluation.failed")

	require.Eventually(t, func() bool { return assert.ObjectsAreEqual([]int{0, 0, 0, 0, 2}, counts(adm, qs...)) },
		5*time.Second, 100*time.Millisecond)
	time.Sleep(500 * time.Millisecond)
	assert.Equal(t, []int{0, 0, 0, 0, 2}, counts(adm, qs...))
}

func TestRetryQueue_5s_ReturnsOnlyToOriginQueue(t *testing.T) {
	ch, adm := topologyEnv(t)
	origin := events.QueueEvaluation
	others := []string{events.QueueMemory, events.QueueSummary, events.QueueLearner}
	retry := amqp.RetryQueueName(origin, amqp.RetryLevels[0])

	start := time.Now()
	publish(t, ch, amqp.ExchangeRetry, retry)

	require.Eventually(t, func() bool { return adm.QueueMessages(retry) == 1 }, 3*time.Second, 50*time.Millisecond)
	for time.Since(start) < 4*time.Second {
		assert.Equal(t, 0, adm.QueueMessages(origin), "must not return before ~5s")
		time.Sleep(500 * time.Millisecond)
	}
	require.Eventually(t, func() bool { return adm.QueueMessages(origin) == 1 }, 9*time.Second-time.Since(start), 100*time.Millisecond)
	assert.Equal(t, 0, adm.QueueMessages(retry))
	assert.Equal(t, []int{0, 0, 0}, counts(adm, others...), "requeue goes to the origin queue only")
}
