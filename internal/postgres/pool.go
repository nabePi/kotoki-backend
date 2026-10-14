// Package postgres holds the PostgreSQL adapter: pool factory, query tracer,
// sqlc-generated queries and the transactional Store.
package postgres

import (
	"context"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/nabePi/kotoki-backend/internal/platform/config"
	"github.com/nabePi/kotoki-backend/internal/platform/errs"
	"github.com/nabePi/kotoki-backend/internal/platform/metrics"
)

// PoolOptions carries per-binary settings for NewPool.
type PoolOptions struct {
	ServiceName string
	Metrics     *metrics.DB
}

func ms(d time.Duration) string { return strconv.FormatInt(d.Milliseconds(), 10) }

// NewPool builds a pgx pool with server-side timeouts, the query tracer and
// the sizing from cfg. The pool connects lazily; readiness checks ping it.
func NewPool(ctx context.Context, cfg config.DB, o PoolOptions) (*pgxpool.Pool, error) {
	pc, err := pgxpool.ParseConfig(cfg.URL.Reveal())
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeServiceUnavailable, "database configuration is invalid")
	}
	pc.MaxConns, pc.MinConns = cfg.MaxConns, cfg.MinConns
	pc.MaxConnLifetime, pc.MaxConnIdleTime = cfg.MaxConnLifetime, cfg.MaxConnIdleTime
	pc.ConnConfig.ConnectTimeout = cfg.ConnectTimeout
	pc.ConnConfig.RuntimeParams["statement_timeout"] = ms(cfg.StatementTimeout)
	pc.ConnConfig.RuntimeParams["lock_timeout"] = ms(cfg.LockTimeout)
	pc.ConnConfig.RuntimeParams["idle_in_transaction_session_timeout"] = ms(cfg.IdleInTxTimeout)
	pc.ConnConfig.RuntimeParams["application_name"] = o.ServiceName
	pc.ConnConfig.Tracer = &queryTracer{metrics: o.Metrics}

	pool, err := pgxpool.NewWithConfig(ctx, pc)
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeServiceUnavailable, "database pool could not be created")
	}
	return pool, nil
}

// PoolStat adapts a pool snapshot to the metrics gauge callback.
func PoolStat(p *pgxpool.Pool) metrics.PoolStat {
	s := p.Stat()
	return metrics.PoolStat{Acquired: s.AcquiredConns(), Idle: s.IdleConns(), Max: s.MaxConns()}
}
