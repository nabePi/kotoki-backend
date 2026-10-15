package postgres

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/nabePi/kotoki-backend/internal/balance"
	"github.com/nabePi/kotoki-backend/internal/platform/errs"
	"github.com/nabePi/kotoki-backend/internal/postgres/sqlcgen"
)

type reconcileRepo struct{ q *sqlcgen.Queries }

// NewReconcileRepo returns the read-only R1–R6 queries of spec 4.8 over the pool. The M6 job runs
// them (statement_timeout 60 s); nothing here fixes data.
func NewReconcileRepo(pool *pgxpool.Pool) balance.ReconcileRepository {
	return &reconcileRepo{q: sqlcgen.New(pool)}
}

// RunCheck runs one named check and returns its offending rows (ids and numbers only).
func (r *reconcileRepo) RunCheck(ctx context.Context, check string) ([]string, error) {
	var rows []string
	var err error
	switch check {
	case "R1":
		rows, err = r.q.ReconcileR1(ctx)
	case "R2":
		rows, err = r.q.ReconcileR2(ctx)
	case "R3":
		rows, err = r.q.ReconcileR3(ctx)
	case "R4":
		rows, err = r.q.ReconcileR4(ctx)
	case "R5":
		rows, err = r.q.ReconcileR5(ctx)
	case "R6":
		rows, err = r.q.ReconcileR6(ctx)
	default:
		return nil, fmt.Errorf("postgres: unknown reconcile check %q", check)
	}
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeInternalError, "reconcile query failed")
	}
	return rows, nil
}
