package admin

import (
	"context"

	"github.com/nabePi/kotoki-backend/internal/auth"
	"github.com/nabePi/kotoki-backend/internal/balance"
)

// Store runs admin transactions.
type Store interface {
	WithinTx(ctx context.Context, fn func(ctx context.Context, tx Tx) error) error
}

// Tx combines the repositories admin use cases touch in one transaction.
type Tx interface {
	auth.Tx    // Users, RefreshTokens, PasswordResets, Accounts
	balance.Tx // Balance
	Audit() AuditRepository
	SessionGate() SessionGate
}
