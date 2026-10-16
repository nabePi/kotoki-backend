// Package amqp is the RabbitMQ adapter: topology, reconnecting connection, confirm publisher,
// outbox publisher and the consumer framework (spec chapter 9). The client library is imported
// as amqp091.
package amqp

import (
	"fmt"
	"time"

	amqp091 "github.com/rabbitmq/amqp091-go"

	"github.com/nabePi/kotoki-backend/internal/events"
)

// Exchange and queue names (spec 9.6.1).
const (
	ExchangeEvents   = "kotoki.events"
	ExchangeUnrouted = "kotoki.events.unrouted"
	ExchangeRetry    = "kotoki.retry"
	ExchangeRequeue  = "kotoki.requeue"
	ExchangeDLX      = "kotoki.dlx"
	QueueUnrouted    = "kotoki.events.unrouted"
)

// RetryLevel is one retry queue: Suffix names it, TTL is how long a message waits there.
type RetryLevel struct {
	Suffix string
	TTL    time.Duration
}

// RetryLevels is indexed by the x-retry-count value BEFORE the retry (0 -> 5s, 1 -> 30s, 2 -> 2m, 3 -> 10m).
var RetryLevels = []RetryLevel{
	{"5s", 5 * time.Second}, {"30s", 30 * time.Second},
	{"2m", 2 * time.Minute}, {"10m", 10 * time.Minute},
}

// MaxRetries is the number of retries after the first attempt (D-092).
const MaxRetries = 4

// QueueSpec describes a main queue: its bindings on kotoki.events and the per-pod prefetch.
type QueueSpec struct {
	Name        string
	RoutingKeys []string
	Prefetch    int
}

// Queues lists the main queues (spec 9.6.2).
var Queues = []QueueSpec{
	{events.QueueEvaluation, []string{"session.completed"}, 4},
	{events.QueueMemory, []string{"session.completed"}, 4},
	{events.QueueSummary, []string{"session.completed"}, 8},
	{events.QueueLearner, []string{"evaluation.completed"}, 16},
}

// RetryQueueName is the retry queue of q at level lvl.
func RetryQueueName(q string, lvl RetryLevel) string { return q + ".retry." + lvl.Suffix }

// DLQName is the dead-letter queue of q.
func DLQName(q string) string { return q + ".dlq" }

// DeclareTopology declares 5 exchanges and 25 quorum queues. It is idempotent and runs at every
// worker start and after every reconnect. RabbitMQ rejects changed queue arguments with
// PRECONDITION_FAILED; changing them means a new queue name (see docs/ops/rabbitmq.md).
func DeclareTopology(ch *amqp091.Channel) error {
	for _, ex := range []struct {
		name, kind string
		args       amqp091.Table
	}{
		{ExchangeUnrouted, "fanout", nil},
		{ExchangeEvents, "topic", amqp091.Table{"alternate-exchange": ExchangeUnrouted}},
		{ExchangeRetry, "direct", nil},
		{ExchangeRequeue, "direct", nil},
		{ExchangeDLX, "direct", nil},
	} {
		if err := ch.ExchangeDeclare(ex.name, ex.kind, true, false, false, false, ex.args); err != nil {
			return fmt.Errorf("amqp: declare exchange %s: %w", ex.name, err)
		}
	}

	declare := func(name string, args amqp091.Table) error {
		if _, err := ch.QueueDeclare(name, true, false, false, false, args); err != nil {
			return fmt.Errorf("amqp: declare queue %s: %w", name, err)
		}
		return nil
	}
	bind := func(queue, key, exchange string) error {
		if err := ch.QueueBind(queue, key, exchange, false, nil); err != nil {
			return fmt.Errorf("amqp: bind %s to %s (%s): %w", queue, exchange, key, err)
		}
		return nil
	}

	if err := declare(QueueUnrouted, amqp091.Table{
		"x-queue-type":  "quorum",
		"x-message-ttl": (72 * time.Hour).Milliseconds(),
		"x-max-length":  int64(100000),
		"x-overflow":    "drop-head",
	}); err != nil {
		return err
	}
	if err := bind(QueueUnrouted, "", ExchangeUnrouted); err != nil {
		return err
	}

	for _, q := range Queues {
		if err := declare(q.Name, amqp091.Table{
			"x-queue-type":              "quorum",
			"x-delivery-limit":          int64(10),
			"x-dead-letter-exchange":    ExchangeDLX,
			"x-dead-letter-routing-key": q.Name,
			"x-dead-letter-strategy":    "at-least-once",
			"x-overflow":                "reject-publish",
		}); err != nil {
			return err
		}
		for _, rk := range q.RoutingKeys {
			if err := bind(q.Name, rk, ExchangeEvents); err != nil {
				return err
			}
		}
		if err := bind(q.Name, q.Name, ExchangeRequeue); err != nil {
			return err
		}

		for _, lvl := range RetryLevels {
			rq := RetryQueueName(q.Name, lvl)
			if err := declare(rq, amqp091.Table{
				"x-queue-type":              "quorum",
				"x-message-ttl":             lvl.TTL.Milliseconds(),
				"x-dead-letter-exchange":    ExchangeRequeue,
				"x-dead-letter-routing-key": q.Name,
				"x-dead-letter-strategy":    "at-least-once",
				"x-overflow":                "reject-publish",
			}); err != nil {
				return err
			}
			if err := bind(rq, rq, ExchangeRetry); err != nil {
				return err
			}
		}

		dlq := DLQName(q.Name)
		if err := declare(dlq, amqp091.Table{"x-queue-type": "quorum"}); err != nil {
			return err
		}
		if err := bind(dlq, q.Name, ExchangeDLX); err != nil {
			return err
		}
	}
	return nil
}
