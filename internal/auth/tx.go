package auth

import "context"

// Store runs auth business transactions.
type Store interface {
	WithinTx(ctx context.Context, fn func(ctx context.Context, tx Tx) error) error
}

// Tx exposes the repositories available inside an auth transaction.
type Tx interface {
	Users() UserRepository
	RefreshTokens() RefreshTokenRepository
	PasswordResets() PasswordResetRepository
	Accounts() AccountOpener
}
