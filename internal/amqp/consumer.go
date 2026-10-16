package amqp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
	amqp091 "github.com/rabbitmq/amqp091-go"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"github.com/nabePi/kotoki-backend/internal/events"
	"github.com/nabePi/kotoki-backend/internal/platform/buildinfo"
	"github.com/nabePi/kotoki-backend/internal/platform/clock"
)

// Consumer headers (spec 9.7.4) and the x-last-error cap.
const (
	headerRetryCount    = "x-retry-count"
	headerOriginalQueue = "x-original-queue"
	headerLastError     = "x-last-error"
	headerFirstFailedAt = "x-first-failed-at"
	headerErrorClass    = "x-error-class"
	headerDeadAt        = "x-dead-at"
	maxLastError        = 512

	codeDecodeError        = "DECODE_ERROR"
	codeUnsupportedVersion = "UNSUPPORTED_EVENT_VERSION"
)

// Consumer result labels of kotoki_consumer_messages_total.
const (
	resultAck       = "ack"
	resultDuplicate = "duplicate"
	resultRetry     = "retry"
	resultDead      = "dead"
	resultRequeue   = "requeue"
	resultSkipped   = "skipped_deleted_user"
)

// Handler is implemented structurally by the feature consumers (evaluation, memory, learner,
// summary); it is defined here, on the consuming side (spec 9.7.1).
type Handler interface {
	// Queue is the queue name and the consumer name in processed_events.
	Queue() string
	// SupportedEvents maps event_type to the highest event_version this handler understands.
	SupportedEvents() map[string]int
	// Timeout bounds one Handle call (spec 9.6.2).
	Timeout() time.Duration
	// Handle performs the business effect; its final write MUST be one transaction that also
	// calls MarkProcessed (D-093).
	Handle(ctx context.Context, d events.Delivery) error
	// OnFailure is called best effort before a retry or dead-lettering; final is true when the
	// message goes to the DLQ. It is not called on shutdown requeue.
	OnFailure(ctx context.Context, d events.Delivery, cause error, final bool)
}

// Publisher publishes with publisher confirms; *ConfirmPublisher satisfies it.
type Publisher interface {
	PublishConfirmed(ctx context.Context, exchange, key string, msg amqp091.Publishing) error
}

// ProcessedChecker is the cheap idempotency pre-check before expensive work (spec 9.7.2). It does
// not replace MarkProcessed.
type ProcessedChecker interface {
	IsProcessed(ctx context.Context, consumer string, eventID uuid.UUID) (bool, error)
}

// Consumer runs one Handler: it decodes, pre-checks, handles with a timeout and routes the outcome
// to ack, retry ladder, DLQ or shutdown requeue (spec 9.7.4).
type Consumer struct {
	h         Handler
	conn      *Conn
	pub       Publisher
	processed ProcessedChecker
	m         *Metrics
	log       *slog.Logger
	clk       clock.Clock

	lifeOnce sync.Once
	lc       *lifecycle
}

// NewConsumer builds the consumer of h on conn; retries and dead letters go out through pub.
func NewConsumer(h Handler, conn *Conn, pub Publisher, processed ProcessedChecker, m *Metrics, log *slog.Logger, clk clock.Clock) *Consumer {
	return &Consumer{h: h, conn: conn, pub: pub, processed: processed, m: m, log: log, clk: clk}
}

// process handles one delivery to completion: it always ends in exactly one Ack or Nack.
func (c *Consumer) process(root context.Context, msg amqp091.Delivery) {
	queue := c.h.Queue()
	start := time.Now()
	c.m.ConsumerInflight.WithLabelValues(queue).Inc()
	defer func() {
		c.m.ConsumerInflight.WithLabelValues(queue).Dec()
		c.m.ConsumerProcessing.WithLabelValues(queue).Observe(time.Since(start).Seconds())
	}()

	retryCount := headerInt(msg.Headers, headerRetryCount)
	env, decodeErr := events.DecodeEnvelope(msg.Body)
	if decodeErr == nil {
		decodeErr = c.checkSupported(env)
	}

	ctx, span := c.startSpan(root, msg, env, decodeErr == nil, retryCount)
	defer span.End()
	log := c.logger(ctx, env, retryCount)

	if decodeErr != nil {
		cause := asPermanent(codeDecodeError, decodeErr)
		span.SetStatus(codes.Error, cause.Error())
		log.WarnContext(ctx, "undecodable delivery, dead-lettering", "error", cause)
		c.dead(ctx, log, msg, cause)
		return
	}

	d := events.Delivery{Envelope: env, Queue: queue, RetryCount: retryCount, Redelivered: msg.Redelivered}

	if done, err := c.processed.IsProcessed(ctx, queue, env.EventID); err == nil && done {
		c.ack(log, msg)
		c.m.ConsumerMessages.WithLabelValues(queue, resultDuplicate).Inc()
		return
	}

	hctx, cancel := context.WithTimeout(ctx, c.h.Timeout())
	err := c.safeHandle(hctx, d)
	cancel()

	switch Classify(err, root.Err() != nil) {
	case ClassAck:
		c.ack(log, msg)
		result := resultAck
		if errors.Is(err, events.ErrSkippedDeletedUser) {
			result = resultSkipped
		}
		c.m.ConsumerMessages.WithLabelValues(queue, result).Inc()
	case ClassRequeue:
		if nerr := msg.Nack(false, true); nerr != nil {
			log.WarnContext(ctx, "nack requeue failed", "error", nerr)
		}
		c.m.ConsumerMessages.WithLabelValues(queue, resultRequeue).Inc()
	case ClassRetry:
		span.SetStatus(codes.Error, err.Error())
		if d.RetryCount >= MaxRetries {
			c.onFailure(ctx, log, d, err, true)
			c.dead(ctx, log, msg, err)
			return
		}
		c.onFailure(ctx, log, d, err, false)
		c.retry(ctx, log, msg, d, err)
	case ClassDead:
		span.SetStatus(codes.Error, err.Error())
		c.onFailure(ctx, log, d, err, true)
		c.dead(ctx, log, msg, err)
	}
}

// checkSupported rejects an event type this handler does not know or a version newer than it
// understands; both are permanent.
func (c *Consumer) checkSupported(env events.Envelope) error {
	maxVersion, ok := c.h.SupportedEvents()[env.EventType]
	if !ok {
		return events.Permanent(codeDecodeError, fmt.Errorf("event_type %q not handled by %s", env.EventType, c.h.Queue()))
	}
	if env.EventVersion > maxVersion {
		return events.Permanent(codeUnsupportedVersion,
			fmt.Errorf("%s v%d not supported (max v%d)", env.EventType, env.EventVersion, maxVersion))
	}
	return nil
}

// startSpan extracts the trace context from the AMQP headers (fallback: the envelope fields) and
// starts the CONSUMER span "<queue> process" (spec 9.7.5).
func (c *Consumer) startSpan(root context.Context, msg amqp091.Delivery, env events.Envelope, decoded bool, retryCount int) (context.Context, trace.Span) {
	prop := propagation.TraceContext{}
	parent := prop.Extract(root, amqpHeaderCarrier(msg.Headers))
	if !trace.SpanContextFromContext(parent).IsValid() && env.Traceparent != "" {
		mc := propagation.MapCarrier{"traceparent": env.Traceparent}
		if env.Tracestate != nil {
			mc["tracestate"] = *env.Tracestate
		}
		parent = prop.Extract(root, mc)
	}
	attrs := []attribute.KeyValue{
		attribute.String("messaging.system", "rabbitmq"),
		attribute.String("messaging.destination.name", c.h.Queue()),
		attribute.Int("kotoki.retry_count", retryCount),
	}
	if decoded {
		attrs = append(attrs,
			attribute.String("messaging.rabbitmq.destination.routing_key", env.EventType),
			attribute.String("messaging.message.id", env.EventID.String()),
			attribute.String("kotoki.user_id", env.UserID.String()))
		if env.AggregateType == events.AggregateSession {
			attrs = append(attrs, attribute.String("kotoki.session_id", env.AggregateID.String()))
		}
	}
	return otel.GetTracerProvider().Tracer(tracerName).Start(parent, c.h.Queue()+" process",
		trace.WithSpanKind(trace.SpanKindConsumer), trace.WithAttributes(attrs...))
}

func (c *Consumer) logger(ctx context.Context, env events.Envelope, retryCount int) *slog.Logger {
	sc := trace.SpanContextFromContext(ctx)
	return c.log.With("trace_id", sc.TraceID().String(), "span_id", sc.SpanID().String(),
		"event_id", env.EventID.String(), "event_type", env.EventType, "queue", c.h.Queue(),
		"retry_count", retryCount, "worker_version", buildinfo.Version)
}

// safeHandle turns a handler panic into an error (class: retry, rule 10).
func (c *Consumer) safeHandle(ctx context.Context, d events.Delivery) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("handler panic: %v", r)
		}
	}()
	return c.h.Handle(ctx, d)
}

// onFailure calls Handler.OnFailure best effort: its panics are contained and never change routing.
func (c *Consumer) onFailure(ctx context.Context, log *slog.Logger, d events.Delivery, cause error, final bool) {
	defer func() {
		if r := recover(); r != nil {
			log.ErrorContext(ctx, "OnFailure panicked", "panic", fmt.Sprint(r))
		}
	}()
	c.h.OnFailure(context.WithoutCancel(ctx), d, cause, final)
}

// retry publishes a copy to the retry queue of the current level, then acks the original. A failed
// publish nacks with requeue so the message is not lost (the broker's delivery limit counts it).
func (c *Consumer) retry(ctx context.Context, log *slog.Logger, msg amqp091.Delivery, d events.Delivery, cause error) {
	lvl := RetryLevels[d.RetryCount]
	h := cloneHeaders(msg.Headers)
	h[headerRetryCount] = int32(d.RetryCount + 1) //nolint:gosec // G115: RetryCount < MaxRetries (4) here
	h[headerOriginalQueue] = d.Queue
	h[headerLastError] = truncate(cause.Error(), maxLastError)
	if _, ok := h[headerFirstFailedAt]; !ok {
		h[headerFirstFailedAt] = c.clk.Now().UTC().Format(time.RFC3339Nano)
	}
	otel.GetTextMapPropagator().Inject(ctx, amqpHeaderCarrier(h))

	pctx := context.WithoutCancel(ctx)
	if err := c.pub.PublishConfirmed(pctx, ExchangeRetry, RetryQueueName(d.Queue, lvl), copyMessage(msg, h)); err != nil {
		log.WarnContext(ctx, "retry publish failed, requeueing", "error", err, "level", lvl.Suffix)
		if nerr := msg.Nack(false, true); nerr != nil {
			log.WarnContext(ctx, "nack requeue failed", "error", nerr)
		}
		return
	}
	c.ack(log, msg)
	c.m.ConsumerMessages.WithLabelValues(d.Queue, resultRetry).Inc()
	log.InfoContext(ctx, "delivery scheduled for retry", "level", lvl.Suffix, "error", cause)
}

// dead publishes a copy to the DLX, then acks the original. A failed publish nacks without requeue
// so the queue-level dead-letter exchange routes it to <queue>.dlq.
func (c *Consumer) dead(ctx context.Context, log *slog.Logger, msg amqp091.Delivery, cause error) {
	queue := c.h.Queue()
	h := cloneHeaders(msg.Headers)
	h[headerOriginalQueue] = queue
	h[headerLastError] = truncate(cause.Error(), maxLastError)
	h[headerErrorClass] = errorCode(cause)
	h[headerDeadAt] = c.clk.Now().UTC().Format(time.RFC3339Nano)

	pctx := context.WithoutCancel(ctx)
	if err := c.pub.PublishConfirmed(pctx, ExchangeDLX, queue, copyMessage(msg, h)); err != nil {
		log.ErrorContext(ctx, "dlq publish failed, falling back to queue-level dead-lettering", "error", err)
		if nerr := msg.Nack(false, false); nerr != nil {
			log.WarnContext(ctx, "nack failed", "error", nerr)
		}
		c.m.ConsumerMessages.WithLabelValues(queue, resultDead).Inc()
		return
	}
	c.ack(log, msg)
	c.m.ConsumerMessages.WithLabelValues(queue, resultDead).Inc()
	log.WarnContext(ctx, "delivery dead-lettered", "error_class", h[headerErrorClass], "error", cause)
}

func (c *Consumer) ack(log *slog.Logger, msg amqp091.Delivery) {
	if err := msg.Ack(false); err != nil {
		log.Warn("ack failed (broker redelivers; handler is idempotent)", "error", err)
	}
}

// cloneHeaders copies the header table (shallow) so a retry never mutates the delivery's headers.
func cloneHeaders(h amqp091.Table) amqp091.Table {
	out := make(amqp091.Table, len(h)+6)
	for k, v := range h {
		out[k] = v
	}
	return out
}

// copyMessage republishes a delivery unchanged except for its headers: the body is never modified.
func copyMessage(msg amqp091.Delivery, h amqp091.Table) amqp091.Publishing {
	return amqp091.Publishing{
		Headers: h, ContentType: msg.ContentType, DeliveryMode: amqp091.Persistent,
		MessageId: msg.MessageId, Type: msg.Type, Timestamp: msg.Timestamp, AppId: msg.AppId, Body: msg.Body,
	}
}
