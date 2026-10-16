package amqp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"time"

	amqp091 "github.com/rabbitmq/amqp091-go"
)

const headerReplayedAt = "x-replayed-at"

// ReplayOptions configures ReplayDLQ.
type ReplayOptions struct {
	// Queue is a main queue (one of Queues); its DLQ is <Queue>.dlq.
	Queue string
	// Limit caps the number of messages handled; zero or negative means the whole DLQ.
	Limit int
	// DryRun lists the messages on Out and leaves every one of them in the DLQ.
	DryRun bool
	// Out receives the dry-run listing; nil discards it.
	Out io.Writer
}

// ReplayDLQ moves messages from <Queue>.dlq back to the main queue through kotoki.requeue with
// x-retry-count reset to 0 (int32) and x-replayed-at set; every other header and the body are kept.
// A message is acked only after the broker confirmed its republish, so a failure never loses one
// (it returns to the DLQ). It returns the number of messages replayed, or listed in a dry run.
//
// Messages are read one at a time with basic.get (an effective prefetch of 1). A dry run keeps
// them unacked while listing and returns them with a single Nack(multiple=true, requeue=true) at the
// end: nacking each message right away would hand the same head message back on the next get.
func ReplayDLQ(ctx context.Context, conn *Conn, pub Publisher, opt ReplayOptions) (int, error) {
	if !slices.ContainsFunc(Queues, func(q QueueSpec) bool { return q.Name == opt.Queue }) {
		return 0, fmt.Errorf("amqp: dlq replay: unknown queue %q", opt.Queue)
	}
	out := opt.Out
	if out == nil {
		out = io.Discard
	}
	ch, err := conn.Channel()
	if err != nil {
		return 0, err
	}
	defer func() { _ = ch.Close() }()

	dlq := DLQName(opt.Queue)
	var n int
	var pending uint64 // delivery tag of the last message a dry run keeps unacked
	if opt.DryRun {
		defer func() {
			if pending != 0 {
				_ = ch.Nack(pending, true, true)
			}
		}()
	}
	for opt.Limit <= 0 || n < opt.Limit {
		if err := ctx.Err(); err != nil {
			return n, err
		}
		msg, ok, err := ch.Get(dlq, false)
		if err != nil {
			return n, fmt.Errorf("amqp: dlq replay: get from %s: %w", dlq, err)
		}
		if !ok {
			break
		}
		if opt.DryRun {
			pending = msg.DeliveryTag
			printDead(out, msg)
			n++
			continue
		}
		h := cloneHeaders(msg.Headers)
		h[headerRetryCount] = int32(0)
		h[headerReplayedAt] = time.Now().UTC().Format(time.RFC3339Nano)
		if err := pub.PublishConfirmed(context.WithoutCancel(ctx), ExchangeRequeue, opt.Queue, copyMessage(msg, h)); err != nil {
			_ = msg.Nack(false, true)
			return n, fmt.Errorf("amqp: dlq replay: republish to %s: %w", opt.Queue, err)
		}
		if err := msg.Ack(false); err != nil {
			// already republished; the broker redelivers the DLQ copy and the handler is idempotent
			return n, fmt.Errorf("amqp: dlq replay: ack: %w", err)
		}
		n++
	}
	return n, nil
}

// printDead writes one dry-run line: event_id, event_type, x-error-class, x-last-error.
func printDead(out io.Writer, msg amqp091.Delivery) {
	id := msg.MessageId
	if id == "" {
		var env struct {
			EventID string `json:"event_id"`
		}
		if json.Unmarshal(msg.Body, &env) == nil {
			id = env.EventID
		}
	}
	_, _ = fmt.Fprintf(out, "event_id=%s event_type=%s x-error-class=%v x-last-error=%q\n",
		id, msg.Type, msg.Headers[headerErrorClass], fmt.Sprint(msg.Headers[headerLastError]))
}
