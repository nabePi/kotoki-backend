//go:build integration

package postgres_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/postgres"
	"github.com/nabePi/kotoki-backend/internal/postgres/sqlcgen"
	"github.com/nabePi/kotoki-backend/internal/testutil"
)

// The reconcile queries run in a READ ONLY transaction with a 60 s statement timeout, so that a
// slow check on a large ledger is cut off and nothing can be written by them.
func TestRunCheck_ReadOnlyTxAndTimeout(t *testing.T) {
	pool := testutil.NewPostgres(t)
	ctx := context.Background()
	for _, check := range []string{"R1", "R2", "R3", "R4", "R5", "R6"} {
		probed := 0
		repo := postgres.NewReconcileRepoWithProbe(pool, func(ctx context.Context, db sqlcgen.DBTX) {
			probed++
			var ro, timeout string
			require.NoError(t, db.QueryRow(ctx, `SHOW transaction_read_only`).Scan(&ro))
			require.NoError(t, db.QueryRow(ctx, `SHOW statement_timeout`).Scan(&timeout))
			require.Equal(t, "on", ro, check)
			require.Equal(t, "1min", timeout, check)
		})
		rows, err := repo.RunCheck(ctx, check)
		require.NoError(t, err, check)
		require.Empty(t, rows, check)
		require.Equal(t, 1, probed, check)
	}

	// the setting is local to the transaction: a pooled connection afterwards has the defaults
	var ro string
	require.NoError(t, pool.QueryRow(ctx, `SHOW transaction_read_only`).Scan(&ro))
	require.Equal(t, "off", ro)
}
