package postgres

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/nabePi/kotoki-backend/internal/admin"
	"github.com/nabePi/kotoki-backend/internal/auth"
	"github.com/nabePi/kotoki-backend/internal/balance"
	"github.com/nabePi/kotoki-backend/internal/session"
)

// balanceStoreView adapts Store to balance.Store: *Tx already satisfies balance.Tx.
type balanceStoreView struct{ s *Store }

// BalanceStore returns the balance.Store view of s.
func (s *Store) BalanceStore() balance.Store { return balanceStoreView{s: s} }

func (v balanceStoreView) WithinTx(ctx context.Context, fn func(ctx context.Context, tx balance.Tx) error) error {
	return v.s.WithinTx(ctx, func(ctx context.Context, tx *Tx) error { return fn(ctx, tx) })
}

// Accounts bridges registration to balance.OpenAccount inside the same transaction.
func (t *Tx) Accounts() auth.AccountOpener { return accountOpener{tx: t} }

type accountOpener struct{ tx *Tx }

func (a accountOpener) OpenAccount(ctx context.Context, userID uuid.UUID, initialSeconds int64, at time.Time) error {
	return balance.OpenAccount(ctx, a.tx, a.tx.IDs(), userID, initialSeconds, at)
}

// authStoreView adapts Store to auth.Store.
type authStoreView struct{ s *Store }

// AuthStore returns the auth.Store view of s.
func (s *Store) AuthStore() auth.Store { return authStoreView{s: s} }

func (v authStoreView) WithinTx(ctx context.Context, fn func(ctx context.Context, tx auth.Tx) error) error {
	return v.s.WithinTx(ctx, func(ctx context.Context, tx *Tx) error { return fn(ctx, tx) })
}

// adminStoreView adapts Store to admin.Store: *Tx combines auth, balance, audit and the session gate.
type adminStoreView struct{ s *Store }

// AdminStore returns the admin.Store view of s.
func (s *Store) AdminStore() admin.Store { return adminStoreView{s: s} }

func (v adminStoreView) WithinTx(ctx context.Context, fn func(ctx context.Context, tx admin.Tx) error) error {
	return v.s.WithinTx(ctx, func(ctx context.Context, tx *Tx) error { return fn(ctx, tx) })
}

var _ admin.Tx = (*Tx)(nil)

type sessionStoreView struct{ s *Store }

// SessionStore returns the session.Store view of s: *Tx satisfies session.Tx (balance, sessions,
// turns, events and the outbox in one transaction).
func (s *Store) SessionStore() session.Store { return sessionStoreView{s: s} }

func (v sessionStoreView) WithinTx(ctx context.Context, fn func(ctx context.Context, tx session.Tx) error) error {
	return v.s.WithinTx(ctx, func(ctx context.Context, tx *Tx) error { return fn(ctx, tx) })
}
