// Package events holds the integration events and the outbox port (spec Bab 9 §9.3). Feature
// packages enqueue events inside their business transaction through an OutboxWriter obtained from
// their transaction port; publishing to the broker is the job of a separate process.
package events

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// Event is one integration event, built by a New* function of this package.
type Event struct {
	EventID       uuid.UUID // UUIDv7, created by New*
	EventType     string    // constants in types.go
	EventVersion  int
	AggregateType string // "SESSION" | "EVALUATION"
	AggregateID   uuid.UUID
	UserID        uuid.UUID
	Producer      string    // "API" | "REALTIME" | "WORKER"
	OccurredAt    time.Time // UTC
	Payload       any       // payload struct, marshalled to JSON
}

// OutboxWriter is the outbox port. Enqueue MUST be called inside the business transaction; the
// implementation copies the trace context of ctx into traceparent/tracestate.
type OutboxWriter interface {
	Enqueue(ctx context.Context, ev Event) error
}
