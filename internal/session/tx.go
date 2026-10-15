package session

import (
	"context"

	"github.com/nabePi/kotoki-backend/internal/balance"
	"github.com/nabePi/kotoki-backend/internal/events"
)

// Store opens a READ COMMITTED transaction with the timeouts and retries of balance.Store and
// calls fn with a Tx bound to it.
type Store interface {
	WithinTx(ctx context.Context, fn func(ctx context.Context, tx Tx) error) error
}

// Tx is the session transaction. It embeds balance.Tx, so the tx-bound operations of
// balance.Service run inside the same transaction (spec 4.2); admin.Tx satisfies it too.
type Tx interface {
	balance.Tx // Balance() balance.Repository
	Sessions() Repository
	Turns() TurnRepository
	Events() EventRepository     // session_events
	Outbox() events.OutboxWriter // spec Bab 9
}
