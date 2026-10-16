package postgres

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/nabePi/kotoki-backend/internal/balance"
	"github.com/nabePi/kotoki-backend/internal/platform/errs"
	"github.com/nabePi/kotoki-backend/internal/postgres/sqlcgen"
)

// reconcileStatementTimeout bounds one check (spec 4.8, 9.9.5): the queries scan whole tables.
const reconcileStatementTimeout = "60s"

type reconcileRepo struct {
	pool  *pgxpool.Pool
	probe func(ctx context.Context, db sqlcgen.DBTX) // tests only: runs in the check's transaction
}

// NewReconcileRepo returns the read-only R1–R6 queries of spec 4.8 over the pool. Each check runs
// in its own READ ONLY transaction with SET LOCAL statement_timeout = 60 s; nothing here fixes data.
func NewReconcileRepo(pool *pgxpool.Pool) balance.ReconcileRepository {
	return &reconcileRepo{pool: pool}
}

// RunCheck runs one named check and returns its offending rows (ids and numbers only).
func (r *reconcileRepo) RunCheck(ctx context.Context, check string) ([]string, error) {
	var run func(*sqlcgen.Queries, context.Context) ([]string, error)
	switch check {
	case "R1":
		run = (*sqlcgen.Queries).ReconcileR1
	case "R2":
		run = (*sqlcgen.Queries).ReconcileR2
	case "R3":
		run = (*sqlcgen.Queries).ReconcileR3
	case "R4":
		run = (*sqlcgen.Queries).ReconcileR4
	case "R5":
		run = (*sqlcgen.Queries).ReconcileR5
	case "R6":
		run = (*sqlcgen.Queries).ReconcileR6
	default:
		return nil, fmt.Errorf("postgres: unknown reconcile check %q", check)
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeInternalError, "reconcile begin failed")
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := tx.Exec(ctx, "SET LOCAL statement_timeout = '"+reconcileStatementTimeout+"'"); err != nil {
		return nil, errs.Wrap(err, errs.CodeInternalError, "reconcile set timeout failed")
	}
	if r.probe != nil {
		r.probe(ctx, tx)
	}
	rows, err := run(sqlcgen.New(tx), ctx)
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeInternalError, "reconcile query failed")
	}
	return rows, nil
}
