//go:build integration

package migrations_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/testutil"
)

func newMigrator(t *testing.T, dsn string) *migrate.Migrate {
	t.Helper()
	root := testutil.ModuleRoot(t)
	m, err := migrate.New("file://"+filepath.Join(root, "migrations"), "pgx5://"+strings.TrimPrefix(strings.TrimPrefix(dsn, "postgresql://"), "postgres://"))
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = m.Close() })
	return m
}

func upFileCount(t *testing.T) int {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(testutil.ModuleRoot(t), "migrations", "*.up.sql"))
	require.NoError(t, err)
	return len(files)
}

func queryStrings(t *testing.T, dsn, sql string) []string {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	require.NoError(t, err)
	defer func() { _ = conn.Close(ctx) }()
	rows, err := conn.Query(ctx, sql)
	require.NoError(t, err)
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		require.NoError(t, rows.Scan(&s))
		out = append(out, s)
	}
	require.NoError(t, rows.Err())
	return out
}

func TestMigrationsUpDownUp(t *testing.T) {
	dsn := testutil.NewEmptyPostgresDSN(t)
	m := newMigrator(t, dsn)
	require.NoError(t, m.Up())
	require.NoError(t, m.Down())

	tables := queryStrings(t, dsn, `SELECT tablename FROM pg_tables WHERE schemaname='public' AND tablename <> 'schema_migrations'`)
	require.Empty(t, tables)
	funcs := queryStrings(t, dsn, `SELECT p.proname FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace
		WHERE n.nspname='public' AND NOT EXISTS (SELECT 1 FROM pg_depend d WHERE d.objid=p.oid AND d.deptype='e')`)
	require.Empty(t, funcs)
	require.Equal(t, []string{"plpgsql"}, queryStrings(t, dsn, `SELECT extname FROM pg_extension ORDER BY 1`))

	require.NoError(t, m.Up())
}

func TestMigrationsStepwise(t *testing.T) {
	dsn := testutil.NewEmptyPostgresDSN(t)
	m := newMigrator(t, dsn)
	for v := 1; v <= upFileCount(t); v++ {
		require.NoError(t, m.Steps(1), "up to v%d", v)
		require.NoError(t, m.Steps(-1), "down from v%d", v)
		require.NoError(t, m.Steps(1), "up again to v%d", v)
	}
}

func TestExtensions(t *testing.T) {
	pool := testutil.NewPostgres(t)
	var n int
	require.NoError(t, pool.QueryRow(context.Background(),
		`SELECT count(*) FROM pg_extension WHERE extname IN ('vector','citext')`).Scan(&n))
	require.Equal(t, 2, n)
}
