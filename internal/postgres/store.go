package postgres

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/nabePi/kotoki-backend/internal/platform/clock"
	"github.com/nabePi/kotoki-backend/internal/platform/errs"
	"github.com/nabePi/kotoki-backend/internal/platform/idgen"
	"github.com/nabePi/kotoki-backend/internal/platform/otel"
	"github.com/nabePi/kotoki-backend/internal/postgres/sqlcgen"
)

const (
	pgDeadlock      = "40P01"
	pgSerialization = "40001"
	pgLockTimeout   = "55P03"
)

// Store owns the pool and runs business transactions. Per-feature views
// (BalanceStore, SessionStore, ...) are added by the feature milestones on top
// of this type.
type Store struct {
	pool      *pgxpool.Pool
	ids       idgen.Generator
	clock     clock.Clock
	lock      time.Duration
	statement time.Duration
	backoff   []time.Duration
	sleep     func(ctx context.Context, d time.Duration) error
}

// StoreOption customises a Store.
type StoreOption func(*Store)

// WithIDs sets the UUID generator handed to repositories (default idgen.NewV7()).
func WithIDs(g idgen.Generator) StoreOption { return func(s *Store) { s.ids = g } }

// WithClock sets the clock handed to repositories (default clock.System{}).
func WithClock(c clock.Clock) StoreOption { return func(s *Store) { s.clock = c } }

// WithTimeouts sets the SET LOCAL lock_timeout and statement_timeout of every
// transaction (default 3s/5s; workers may use 30s).
func WithTimeouts(lock, statement time.Duration) StoreOption {
	return func(s *Store) { s.lock, s.statement = lock, statement }
}

// WithBackoff sets the pauses between retries of deadlock/serialization
// failures; len(d) is the maximum number of retries (default 20ms, 50ms, 100ms).
func WithBackoff(d []time.Duration) StoreOption { return func(s *Store) { s.backoff = d } }

// WithSleep replaces the context-aware sleep used between retries (tests).
func WithSleep(f func(ctx context.Context, d time.Duration) error) StoreOption {
	return func(s *Store) { s.sleep = f }
}

// NewStore returns a Store over pool.
func NewStore(pool *pgxpool.Pool, opts ...StoreOption) *Store {
	s := &Store{
		pool: pool, ids: idgen.NewV7(), clock: clock.System{},
		lock: 3 * time.Second, statement: 5 * time.Second,
		backoff: []time.Duration{20 * time.Millisecond, 50 * time.Millisecond, 100 * time.Millisecond},
		sleep:   sleepCtx,
	}
	for _, o := range opts {
		o(s)
	}
	return s
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Pool returns the underlying pool.
func (s *Store) Pool() *pgxpool.Pool { return s.pool }

// Queries returns non-transactional sqlc queries.
func (s *Store) Queries() *sqlcgen.Queries { return sqlcgen.New(s.pool) }

// IDs returns the UUID generator.
func (s *Store) IDs() idgen.Generator { return s.ids }

// Clock returns the clock.
func (s *Store) Clock() clock.Clock { return s.clock }

// Tx is the handle passed to WithinTx callbacks. Feature milestones (M2, M3,
// M6) add repository and outbox accessors here.
type Tx struct {
	store *Store
	tx    pgx.Tx
}

// Queries returns sqlc queries bound to the transaction.
func (t *Tx) Queries() *sqlcgen.Queries { return sqlcgen.New(t.tx) }

// DBTX returns the transaction as a sqlc DBTX.
func (t *Tx) DBTX() sqlcgen.DBTX { return t.tx }

// IDs returns the Store's UUID generator.
func (t *Tx) IDs() idgen.Generator { return t.store.ids }

// Clock returns the Store's clock.
func (t *Tx) Clock() clock.Clock { return t.store.clock }

func pgCode(err error) string {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.Code
	}
	return ""
}

func retryable(err error) bool {
	c := pgCode(err)
	return c == pgDeadlock || c == pgSerialization
}

// WithinTx runs fn in a READ COMMITTED transaction with SET LOCAL timeouts. It
// rolls back on error or panic (the panic propagates) and retries the whole
// transaction on deadlock (40P01) or serialization failure (40001).
func (s *Store) WithinTx(ctx context.Context, fn func(ctx context.Context, tx *Tx) error) (err error) {
	ctx, span := otel.Start(ctx, "db.tx")
	defer otel.End(span, &err)

	for attempt := 0; ; attempt++ {
		err = s.runOnce(ctx, fn)
		if err == nil || !retryable(err) {
			break
		}
		if attempt >= len(s.backoff) {
			if errs.CodeOf(err) == "" {
				err = errs.Wrap(err, errs.CodeInternalError, "transaction retries exhausted")
			}
			break
		}
		d := time.Duration(float64(s.backoff[attempt]) * (0.5 + rand.Float64())) //nolint:gosec // jitter, not security-sensitive
		if serr := s.sleep(ctx, d); serr != nil {
			err = serr
			break
		}
	}
	if pgCode(err) == pgLockTimeout && errs.CodeOf(err) == "" {
		err = errs.Wrap(err, errs.CodeServiceUnavailable, "lock timeout")
	}
	return err
}

func (s *Store) runOnce(ctx context.Context, fn func(ctx context.Context, tx *Tx) error) (err error) {
	ptx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return err
	}
	done := false
	defer func() {
		if done {
			return
		}
		// Rolling back with a fresh context: ctx may already be cancelled.
		rbCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = ptx.Rollback(rbCtx)
	}()

	if _, err := ptx.Exec(ctx, fmt.Sprintf("SET LOCAL lock_timeout = %d", s.lock.Milliseconds())); err != nil {
		return err
	}
	if _, err := ptx.Exec(ctx, fmt.Sprintf("SET LOCAL statement_timeout = %d", s.statement.Milliseconds())); err != nil {
		return err
	}
	if err := fn(ctx, &Tx{store: s, tx: ptx}); err != nil {
		return err
	}
	if err := ptx.Commit(ctx); err != nil {
		return err
	}
	done = true
	return nil
}
