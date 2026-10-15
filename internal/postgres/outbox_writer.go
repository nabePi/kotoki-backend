package postgres

import (
	"context"
	"encoding/json"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"

	"github.com/nabePi/kotoki-backend/internal/events"
	"github.com/nabePi/kotoki-backend/internal/postgres/sqlcgen"
)

type outboxWriter struct{ q *sqlcgen.Queries }

var _ events.OutboxWriter = outboxWriter{}

// Outbox returns the transaction's outbox writer: INSERT only, so the event commits or rolls back
// with the business change (spec 9.3.1). Publishing is a separate process (M6).
func (t *Tx) Outbox() events.OutboxWriter { return outboxWriter{q: t.Queries()} }

// Enqueue implements events.OutboxWriter; the trace context of ctx is stored in traceparent and
// tracestate so the consumer can continue the trace.
func (w outboxWriter) Enqueue(ctx context.Context, ev events.Event) error {
	payload, err := json.Marshal(ev.Payload)
	if err != nil {
		return internalErr(err, "encode outbox payload failed")
	}
	carrier := propagation.MapCarrier{}
	otel.GetTextMapPropagator().Inject(ctx, carrier)
	var tp, ts *string
	if v := carrier.Get("traceparent"); v != "" {
		tp = &v
	}
	if v := carrier.Get("tracestate"); v != "" {
		ts = &v
	}
	err = w.q.InsertOutboxEvent(ctx, sqlcgen.InsertOutboxEventParams{
		EventID: ev.EventID, EventType: ev.EventType, EventVersion: int32(ev.EventVersion), //nolint:gosec // small constant
		AggregateType: ev.AggregateType, AggregateID: ev.AggregateID, UserID: ev.UserID, Producer: ev.Producer,
		Payload: payload, Traceparent: textPtr(tp), Tracestate: textPtr(ts), OccurredAt: tsz(ev.OccurredAt),
	})
	if err != nil {
		return internalErr(err, "insert outbox event failed")
	}
	return nil
}
