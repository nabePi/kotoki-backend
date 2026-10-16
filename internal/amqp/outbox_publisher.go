package amqp

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	amqp091 "github.com/rabbitmq/amqp091-go"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"github.com/nabePi/kotoki-backend/internal/events"
)

const (
	appID           = "kotoki"
	publishSpanName = "kotoki.events publish"
	tracerName      = "github.com/nabePi/kotoki-backend/internal/amqp"
)

// OutboxRow is one event_outbox row as the publisher reads it (spec 9.3.2).
type OutboxRow struct {
	EventID       uuid.UUID
	EventType     string
	EventVersion  int
	AggregateType string
	AggregateID   uuid.UUID
	UserID        uuid.UUID
	Producer      string
	Payload       json.RawMessage
	Traceparent   *string
	Tracestate    *string
	OccurredAt    time.Time
	Attempts      int
}

// BuildMessage turns an outbox row into the AMQP message of spec 9.3.3. It starts the PRODUCER span
// "kotoki.events publish" whose parent is the trace context stored with the row (no stored context:
// a new root span) and injects that span into the headers and the envelope. end finishes the span;
// call it once the confirm verdict is known.
func BuildMessage(ctx context.Context, row OutboxRow) (amqp091.Publishing, func(), error) {
	// The stored context is the parent, never a span that happens to be active in ctx.
	carrier := propagation.MapCarrier{}
	if row.Traceparent != nil {
		carrier.Set("traceparent", *row.Traceparent)
	}
	if row.Tracestate != nil {
		carrier.Set("tracestate", *row.Tracestate)
	}
	parent := propagation.TraceContext{}.Extract(trace.ContextWithSpanContext(ctx, trace.SpanContext{}), carrier)
	spanCtx, span := otel.GetTracerProvider().Tracer(tracerName).Start(parent, publishSpanName,
		trace.WithSpanKind(trace.SpanKindProducer))
	end := func() { span.End() }

	headers := amqp091.Table{"x-event-version": int32(row.EventVersion)} //nolint:gosec // small constant version
	propagation.TraceContext{}.Inject(spanCtx, amqpHeaderCarrier(headers))
	traceparent, _ := headers["traceparent"].(string)
	var tracestate *string
	if v, ok := headers["tracestate"].(string); ok && v != "" {
		tracestate = &v
	}

	env := events.Envelope{
		EventID: row.EventID, EventType: row.EventType, EventVersion: row.EventVersion,
		OccurredAt: row.OccurredAt.UTC().Truncate(time.Millisecond), Producer: row.Producer,
		AggregateType: row.AggregateType, AggregateID: row.AggregateID, UserID: row.UserID,
		Traceparent: traceparent, Tracestate: tracestate, Payload: row.Payload,
	}
	body, err := json.Marshal(env)
	if err != nil {
		end()
		return amqp091.Publishing{}, nil, fmt.Errorf("amqp: encode envelope %s: %w", row.EventID, err)
	}
	return amqp091.Publishing{
		ContentType:  "application/json",
		DeliveryMode: amqp091.Persistent,
		MessageId:    row.EventID.String(),
		Type:         row.EventType,
		Timestamp:    row.OccurredAt,
		AppId:        appID,
		Headers:      headers,
		Body:         body,
	}, end, nil
}
