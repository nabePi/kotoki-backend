package postgres

import (
	"context"

	"github.com/nabePi/kotoki-backend/internal/balance"
)

// balanceStoreView adapts Store to balance.Store: *Tx already satisfies balance.Tx.
type balanceStoreView struct{ s *Store }

// BalanceStore returns the balance.Store view of s.
func (s *Store) BalanceStore() balance.Store { return balanceStoreView{s: s} }

func (v balanceStoreView) WithinTx(ctx context.Context, fn func(ctx context.Context, tx balance.Tx) error) error {
	return v.s.WithinTx(ctx, func(ctx context.Context, tx *Tx) error { return fn(ctx, tx) })
}
