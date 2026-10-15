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
	"github.com/nabePi/kotoki-backend/internal/session"
)

type sessionGate struct {
	q  *sqlcgen.Queries
	tx *Tx
}

// SessionCanceller is what the admin session gate calls to cancel a RESERVED session inside the
// admin transaction: *session.Service. It is injected with Store.SetSessionCanceller because the
// Service itself is built on top of the Store.
type SessionCanceller interface {
	CancelInTx(ctx context.Context, tx session.Tx, sessionID uuid.UUID, actor session.Actor) (session.Session, error)
}

// SetSessionCanceller wires the session service into the admin gate; call it once at startup,
// before the Store serves requests.
func (s *Store) SetSessionCanceller(c SessionCanceller) { s.canceller = c }

var _ admin.SessionGate = (*sessionGate)(nil)

// SessionGate returns the transaction's view onto sessions for admin use cases.
func (t *Tx) SessionGate() admin.SessionGate { return &sessionGate{q: t.Queries(), tx: t} }

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

// CancelReserved cancels a RESERVED session of the user being deleted and releases its reservation,
// in the admin transaction (spec 3.13.3 step 3). The time comes from the session service's clock.
func (g *sessionGate) CancelReserved(ctx context.Context, sessionID uuid.UUID, _ time.Time) error {
	if g.tx.store.canceller == nil {
		return errs.New(errs.CodeInternalError, "session canceller is not configured")
	}
	if _, err := g.tx.store.canceller.CancelInTx(ctx, g.tx, sessionID, session.ActorAdmin); err != nil {
		return err
	}
	return nil
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
