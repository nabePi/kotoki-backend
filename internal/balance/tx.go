package balance

import "context"

// Store runs balance transactions: READ COMMITTED, lock_timeout 3s, statement_timeout 5s,
// commit on nil error, rollback otherwise, retry on deadlock/serialization failure.
type Store interface {
	WithinTx(ctx context.Context, fn func(ctx context.Context, tx Tx) error) error
}

// Tx exposes the balance repository inside a transaction.
type Tx interface {
	Balance() Repository
}
