package postgres

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/nabePi/kotoki-backend/internal/balance"
	"github.com/nabePi/kotoki-backend/internal/postgres/sqlcgen"
)

// NewReconcileRepoWithProbe is NewReconcileRepo with a probe that runs on the very transaction
// RunCheck executes its query in, right before the query.
func NewReconcileRepoWithProbe(pool *pgxpool.Pool, probe func(ctx context.Context, db sqlcgen.DBTX)) balance.ReconcileRepository {
	return &reconcileRepo{pool: pool, probe: probe}
}
