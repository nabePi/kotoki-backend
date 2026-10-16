package jobs

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// unlockTimeout bounds the unlock round trip, which runs detached from the (possibly cancelled) run context.
const unlockTimeout = 5 * time.Second

// Locker runs fn only when no other instance holds the advisory lock key.
type Locker interface {
	// RunExclusive reports ran=false (and a nil error) when another instance holds key.
	RunExclusive(ctx context.Context, key int64, fn func(context.Context) error) (ran bool, err error)
}

// PgLocker is the Locker on a PostgreSQL session-level advisory lock (spec 9.9.1). The lock lives
// on one pooled connection for the duration of fn, so the pool must connect to PostgreSQL directly,
// not through a transaction-pooling proxy.
type PgLocker struct{ Pool *pgxpool.Pool }

// RunExclusive takes pg_try_advisory_lock(key) without blocking, runs fn, and unlocks on a context
// detached from ctx so a cancelled run still releases the lock. When the unlock cannot be confirmed
// the connection is closed instead of returned to the pool: the server then drops the session lock,
// which would otherwise stay held by an idle pooled connection.
func (l PgLocker) RunExclusive(ctx context.Context, key int64, fn func(context.Context) error) (bool, error) {
	conn, err := l.Pool.Acquire(ctx)
	if err != nil {
		return false, fmt.Errorf("jobs: acquire connection: %w", err)
	}
	released := false
	defer func() {
		if !released {
			conn.Release()
		}
	}()

	var ok bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, key).Scan(&ok); err != nil {
		// the lock request may have reached the server: do not let the connection keep it
		released = true
		_ = conn.Hijack().Close(context.WithoutCancel(ctx))
		return false, fmt.Errorf("jobs: try advisory lock %d: %w", key, err)
	}
	if !ok {
		return false, nil // another instance is running it
	}

	runErr := fn(ctx)

	uctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), unlockTimeout)
	defer cancel()
	var unlocked bool
	if err := conn.QueryRow(uctx, `SELECT pg_advisory_unlock($1)`, key).Scan(&unlocked); err != nil || !unlocked {
		released = true
		_ = conn.Hijack().Close(uctx)
	}
	return true, runErr
}
