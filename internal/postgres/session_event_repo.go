package postgres

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/nabePi/kotoki-backend/internal/postgres/sqlcgen"
	"github.com/nabePi/kotoki-backend/internal/session"
)

type eventRepo struct{ q *sqlcgen.Queries }

var _ session.EventRepository = (*eventRepo)(nil)

// NewEventRepo returns a session.EventRepository over the pool; lifecycle events are normally
// written through (*Tx).Events() inside the transition's transaction.
func NewEventRepo(pool *pgxpool.Pool) session.EventRepository {
	return &eventRepo{q: sqlcgen.New(pool)}
}

// Events returns the transaction's session_events repository.
func (t *Tx) Events() session.EventRepository { return &eventRepo{q: t.Queries()} }

func statusText(s *session.Status) pgtype.Text {
	if s == nil {
		return pgtype.Text{}
	}
	return pgtype.Text{String: string(*s), Valid: true}
}

func eventPayload(e session.Event) []byte {
	if len(e.Payload) == 0 || !json.Valid(e.Payload) {
		return []byte("{}")
	}
	return e.Payload
}

func (r *eventRepo) Append(ctx context.Context, e session.Event) error {
	err := r.q.InsertSessionEvent(ctx, sqlcgen.InsertSessionEventParams{
		ID: e.ID, SessionID: e.SessionID, EventType: string(e.Type), Actor: string(e.Actor),
		FromStatus: statusText(e.FromStatus), ToStatus: statusText(e.ToStatus), TurnID: nullUUID(e.TurnID),
		Payload: eventPayload(e), CreatedAt: tsz(e.CreatedAt),
	})
	if err != nil {
		return internalErr(err, "insert session event failed")
	}
	return nil
}

func (r *eventRepo) AppendFenced(ctx context.Context, fence int64, e session.Event) error {
	n, err := r.q.InsertSessionEventFenced(ctx, sqlcgen.InsertSessionEventFencedParams{
		ID: e.ID, SessionID: e.SessionID, EventType: string(e.Type), Actor: string(e.Actor),
		FromStatus: statusText(e.FromStatus), ToStatus: statusText(e.ToStatus), TurnID: nullUUID(e.TurnID),
		Payload: eventPayload(e), CreatedAt: tsz(e.CreatedAt), Fence: fence,
	})
	return fenced(n, err, "append session event")
}
