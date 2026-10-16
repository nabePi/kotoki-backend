package events

import (
	"context"
	"errors"
)

// ErrAlreadyProcessed is returned by MarkProcessed when (consumer, event_id) is already recorded.
// The caller rolls back and returns it; the framework ACKs (spec 9.7.2).
var ErrAlreadyProcessed = errors.New("event already processed")

// ProcessedEvents is the idempotency port, obtained from the consumer's transaction port so it
// shares the transaction of the business effect (D-093). MarkProcessed MUST be called inside that
// transaction, preferably first.
type ProcessedEvents interface {
	MarkProcessed(ctx context.Context, consumer string, ev Envelope) error
}
