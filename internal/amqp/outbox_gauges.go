package amqp

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	amqp091 "github.com/rabbitmq/amqp091-go"
)

const (
	outboxGaugeEvery = 15 * time.Second
	queueDepthEvery  = 30 * time.Second
)

// RefreshOutboxGauges sets kotoki_outbox_pending, _oldest_pending_age_seconds and _failed from the
// table. The age is measured from created_at, so a row parked in backoff still counts as lag.
func RefreshOutboxGauges(ctx context.Context, pool *pgxpool.Pool, m *Metrics) error {
	var pending, failed int64
	var oldest float64
	err := pool.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE status = 'PENDING'),
		       count(*) FILTER (WHERE status = 'FAILED'),
		       COALESCE(EXTRACT(EPOCH FROM now() - min(created_at) FILTER (WHERE status = 'PENDING')), 0)::float8
		FROM event_outbox
		WHERE status IN ('PENDING', 'FAILED')`).Scan(&pending, &failed, &oldest)
	if err != nil {
		return fmt.Errorf("amqp: outbox gauges: %w", err)
	}
	m.OutboxPending.Set(float64(pending))
	m.OutboxFailed.Set(float64(failed))
	m.OutboxOldestPendingAge.Set(max(oldest, 0))
	return nil
}

// RefreshQueueDepth sets kotoki_rabbitmq_queue_messages{queue} with a passive declare per queue.
// A missing queue closes the channel, so the first error ends the refresh.
func RefreshQueueDepth(_ context.Context, ch *amqp091.Channel, queues []string, m *Metrics) error {
	for _, q := range queues {
		info, err := ch.QueueDeclarePassive(q, true, false, false, false, nil)
		if err != nil {
			return fmt.Errorf("amqp: queue depth %s: %w", q, err)
		}
		m.QueueMessages.WithLabelValues(q).Set(float64(info.Messages))
	}
	return nil
}

// RunOutboxGauges refreshes the outbox gauges every 15 s until ctx ends. onErr, if set, receives
// refresh errors.
func RunOutboxGauges(ctx context.Context, pool *pgxpool.Pool, m *Metrics, onErr func(error)) {
	tick(ctx, outboxGaugeEvery, func() error { return RefreshOutboxGauges(ctx, pool, m) }, onErr)
}

// RunQueueDepth refreshes the queue depth gauge every 30 s until ctx ends; open supplies a fresh
// channel per round, so a dead connection only fails that round.
func RunQueueDepth(ctx context.Context, open func() (*amqp091.Channel, error), queues []string, m *Metrics, onErr func(error)) {
	tick(ctx, queueDepthEvery, func() error {
		ch, err := open()
		if err != nil {
			return err
		}
		defer func() { _ = ch.Close() }()
		return RefreshQueueDepth(ctx, ch, queues, m)
	}, onErr)
}

func tick(ctx context.Context, every time.Duration, fn func() error, onErr func(error)) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if err := fn(); err != nil && onErr != nil && ctx.Err() == nil {
			onErr(err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
