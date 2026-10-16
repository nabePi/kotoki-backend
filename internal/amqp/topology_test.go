package amqp_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/amqp"
	"github.com/nabePi/kotoki-backend/internal/events"
)

func TestQueuesTable(t *testing.T) {
	type row struct {
		name     string
		keys     []string
		prefetch int
	}
	var got []row
	for _, q := range amqp.Queues {
		got = append(got, row{q.Name, q.RoutingKeys, q.Prefetch})
	}
	assert.Equal(t, []row{
		{events.QueueEvaluation, []string{"session.completed"}, 4},
		{events.QueueMemory, []string{"session.completed"}, 4},
		{events.QueueSummary, []string{"session.completed"}, 8},
		{events.QueueLearner, []string{"evaluation.completed"}, 16},
	}, got)
}

func TestRetryLevels(t *testing.T) {
	require.Len(t, amqp.RetryLevels, 4)
	wantMS := []int64{5000, 30000, 120000, 600000}
	wantName := []string{"Q.retry.5s", "Q.retry.30s", "Q.retry.2m", "Q.retry.10m"}
	for i, lvl := range amqp.RetryLevels {
		assert.Equal(t, wantName[i], amqp.RetryQueueName("Q", lvl))
		assert.Equal(t, wantMS[i], lvl.TTL.Milliseconds())
	}
	assert.Equal(t, 5*time.Second, amqp.RetryLevels[0].TTL)
	assert.Equal(t, 4, amqp.MaxRetries)
}

func TestDLQName(t *testing.T) {
	assert.Equal(t, "evaluation.session_completed.dlq", amqp.DLQName(events.QueueEvaluation))
}
