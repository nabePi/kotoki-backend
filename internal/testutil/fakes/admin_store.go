package fakes

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/nabePi/kotoki-backend/internal/admin"
	"github.com/nabePi/kotoki-backend/internal/auth"
	"github.com/nabePi/kotoki-backend/internal/balance"
)

var auditActions = []string{
	"BALANCE_GRANT", "BALANCE_ADJUSTMENT", "USER_STATUS_CHANGE", "SESSION_TRANSCRIPT_VIEW",
	"CONTENT_VERSION_CREATED", "CONTENT_VERSION_PUBLISHED", "CONTENT_VERSION_RETIRED",
	"CONTENT_VERSION_DELETED", "CONTENT_STATUS_CHANGED", "CHARACTER_UPDATED",
}

// SessionGate is an in-memory admin.SessionGate.
type SessionGate struct {
	FailInjector
	Rec       *CallRecorder
	mu        sync.Mutex
	open      map[uuid.UUID][]admin.OpenSession
	cancelled []uuid.UUID
}

var _ admin.SessionGate = (*SessionGate)(nil)

// NewSessionGate returns a gate with no open sessions.
func NewSessionGate() *SessionGate { return &SessionGate{open: map[uuid.UUID][]admin.OpenSession{}} }

// SetOpen replaces the open sessions of userID.
func (g *SessionGate) SetOpen(userID uuid.UUID, sessions ...admin.OpenSession) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.open[userID] = sessions
}

// Cancelled returns the sessions CancelReserved was called for, in order.
func (g *SessionGate) Cancelled() []uuid.UUID {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.cancelled)
}

// OpenSessions lists the configured sessions.
func (g *SessionGate) OpenSessions(_ context.Context, userID uuid.UUID) ([]admin.OpenSession, error) {
	g.Rec.Record("SessionGate.OpenSessions")
	if err := g.Take("SessionGate.OpenSessions"); err != nil {
		return nil, err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.open[userID]), nil
}

// CancelReserved records the cancellation and drops the session from the open list.
func (g *SessionGate) CancelReserved(_ context.Context, sessionID uuid.UUID, _ time.Time) error {
	g.Rec.Record("SessionGate.CancelReserved")
	if err := g.Take("SessionGate.CancelReserved"); err != nil {
		return err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.cancelled = append(g.cancelled, sessionID)
	for u, list := range g.open {
		g.open[u] = slices.DeleteFunc(slices.Clone(list), func(o admin.OpenSession) bool { return o.ID == sessionID })
	}
	return nil
}

func (g *SessionGate) snapshot() (map[uuid.UUID][]admin.OpenSession, []uuid.UUID) {
	g.mu.Lock()
	defer g.mu.Unlock()
	open := make(map[uuid.UUID][]admin.OpenSession, len(g.open))
	for k, v := range g.open {
		open[k] = slices.Clone(v)
	}
	return open, slices.Clone(g.cancelled)
}

func (g *SessionGate) restore(open map[uuid.UUID][]admin.OpenSession, cancelled []uuid.UUID) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.open, g.cancelled = open, cancelled
}

// AdminStore is an in-memory admin.Store layered over an AuthStore and BalanceStore:
// one transaction covers users, tokens, balance, audit rows and the session gate.
type AdminStore struct {
	FailInjector
	Rec  *CallRecorder
	mu   sync.Mutex
	auth *AuthStore
	bal  *BalanceStore
	gate *SessionGate
	rows []admin.AuditLog
}

var _ admin.Store = (*AdminStore)(nil)

// NewAdminStore wires a fresh gate and a shared call recorder into the given stores.
func NewAdminStore(a *AuthStore, b *BalanceStore) *AdminStore {
	rec := &CallRecorder{}
	gate := NewSessionGate()
	a.Rec, b.Rec, gate.Rec = rec, rec, rec
	return &AdminStore{Rec: rec, auth: a, bal: b, gate: gate}
}

// Gate returns the session gate used by every transaction.
func (s *AdminStore) Gate() *SessionGate { return s.gate }

// Audit returns all audit rows in insertion order.
func (s *AdminStore) Audit() []admin.AuditLog {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.rows)
}

// WithinTx runs fn and restores audit rows, gate, users, tokens and balances when it fails.
func (s *AdminStore) WithinTx(ctx context.Context, fn func(ctx context.Context, tx admin.Tx) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows := slices.Clone(s.rows)
	open, cancelled := s.gate.snapshot()
	err := s.auth.within(ctx, func(ctx context.Context, atx *authTx) error {
		return fn(ctx, &adminTx{authTx: atx, s: s})
	})
	if err != nil {
		s.rows = rows
		s.gate.restore(open, cancelled)
	}
	return err
}

type adminTx struct {
	*authTx
	s *AdminStore
}

func (t *adminTx) Balance() balance.Repository    { return t.btx.Balance() }
func (t *adminTx) Audit() admin.AuditRepository   { return &auditRepo{s: t.s} }
func (t *adminTx) SessionGate() admin.SessionGate { return t.s.gate }

var _ auth.Tx = (*adminTx)(nil)

type auditRepo struct{ s *AdminStore }

func (r *auditRepo) Insert(_ context.Context, a admin.AuditLog) error {
	if err := r.s.Take("Audit.Insert"); err != nil {
		return err
	}
	if !slices.Contains(auditActions, a.Action) {
		return errors.New("fakes: audit action violates chk_admin_audit_action")
	}
	r.s.rows = append(r.s.rows, a)
	return nil
}

func (r *auditRepo) List(_ context.Context, f admin.AuditFilter, after *uuid.UUID, limit int) ([]admin.AuditLog, error) {
	var out []admin.AuditLog
	for _, a := range r.s.rows {
		switch {
		case f.AdminUserID != nil && a.AdminUserID != *f.AdminUserID:
		case f.TargetUserID != nil && (a.TargetUserID == nil || *a.TargetUserID != *f.TargetUserID):
		case f.Action != "" && a.Action != f.Action:
		case after != nil && bytes.Compare(a.ID[:], after[:]) >= 0:
		default:
			out = append(out, a)
		}
	}
	slices.SortFunc(out, func(a, b admin.AuditLog) int { return bytes.Compare(b.ID[:], a.ID[:]) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}
