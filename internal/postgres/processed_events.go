package postgres

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/nabePi/kotoki-backend/internal/events"
	"github.com/nabePi/kotoki-backend/internal/postgres/sqlcgen"
)

type processedEventsTx struct{ q *sqlcgen.Queries }

var _ events.ProcessedEvents = processedEventsTx{}

// MarkProcessed implements events.ProcessedEvents: INSERT … ON CONFLICT DO NOTHING; no row inserted
// means the (consumer, event_id) pair was recorded before, reported as ErrAlreadyProcessed. Any
// other failure (including the consumer CHECK) is an internal error.
func (p processedEventsTx) MarkProcessed(ctx context.Context, consumer string, ev events.Envelope) error {
	n, err := p.q.MarkEventProcessed(ctx, sqlcgen.MarkEventProcessedParams{Consumer: consumer, EventID: ev.EventID, EventType: ev.EventType})
	if err != nil {
		return internalErr(err, "mark event processed failed")
	}
	if n == 0 {
		return events.ErrAlreadyProcessed
	}
	return nil
}

// ProcessedEventsReader answers the cheap "already processed?" pre-check outside any transaction.
// It never replaces MarkProcessed.
type ProcessedEventsReader struct{ q *sqlcgen.Queries }

// NewProcessedEventsReader returns a reader over pool.
func NewProcessedEventsReader(pool *pgxpool.Pool) *ProcessedEventsReader {
	return &ProcessedEventsReader{q: sqlcgen.New(pool)}
}

// IsProcessed reports whether consumer has a processed_events row for eventID.
func (r *ProcessedEventsReader) IsProcessed(ctx context.Context, consumer string, eventID uuid.UUID) (bool, error) {
	ok, err := r.q.IsEventProcessed(ctx, sqlcgen.IsEventProcessedParams{Consumer: consumer, EventID: eventID})
	if err != nil {
		return false, internalErr(err, "check processed event failed")
	}
	return ok, nil
}
