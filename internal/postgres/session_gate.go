package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/nabePi/kotoki-backend/internal/admin"
	"github.com/nabePi/kotoki-backend/internal/auth"
	"github.com/nabePi/kotoki-backend/internal/platform/errs"
	"github.com/nabePi/kotoki-backend/internal/postgres/sqlcgen"
)

type sessionGate struct{ q *sqlcgen.Queries }

var _ admin.SessionGate = (*sessionGate)(nil)

// SessionGate returns the transaction's view onto sessions for admin use cases.
func (t *Tx) SessionGate() admin.SessionGate { return &sessionGate{q: t.Queries()} }

// OpenSessions lists the user's sessions that are not terminal.
func (g *sessionGate) OpenSessions(ctx context.Context, userID uuid.UUID) ([]admin.OpenSession, error) {
	rows, err := g.q.ListOpenSessions(ctx, userID)
	if err != nil {
		return nil, internalErr(err, "list open sessions failed")
	}
	out := make([]admin.OpenSession, len(rows))
	for i, r := range rows {
		out[i] = admin.OpenSession{ID: r.ID, Status: r.Status}
	}
	return out, nil
}

// CancelReserved is a deliberate stub until M3 wires session.Service.CancelInTx.
// Open RESERVED sessions cannot exist before M3, so this is only reachable by a bug.
func (g *sessionGate) CancelReserved(context.Context, uuid.UUID, time.Time) error {
	return errs.New(errs.CodeInternalError, "session cancel not wired before M3")
}

// TicketSessionReader implements auth.TicketSessionReader on the pool.
type TicketSessionReader struct{ q *sqlcgen.Queries }

var _ auth.TicketSessionReader = (*TicketSessionReader)(nil)

// NewTicketSessionReader returns a reader over pool.
func NewTicketSessionReader(pool *pgxpool.Pool) *TicketSessionReader {
	return &TicketSessionReader{q: sqlcgen.New(pool)}
}

// GetTicketTarget reads the session status and its reservation deadline (spec 3.11 step 3).
func (r *TicketSessionReader) GetTicketTarget(ctx context.Context, sessionID, userID uuid.UUID) (auth.TicketTarget, error) {
	row, err := r.q.GetTicketTarget(ctx, sqlcgen.GetTicketTargetParams{ID: sessionID, UserID: userID})
	if errors.Is(err, pgx.ErrNoRows) {
		return auth.TicketTarget{}, auth.ErrSessionNotFound
	}
	if err != nil {
		return auth.TicketTarget{}, internalErr(err, "read ticket target failed")
	}
	return auth.TicketTarget{Status: row.Status, StartDeadlineAt: fromTsz(row.StartDeadlineAt)}, nil
}
