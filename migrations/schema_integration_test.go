//go:build integration

package migrations_test

import (
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/testutil"
)

var update = flag.Bool("update", false, "regenerate migrations/testdata/schema.golden.sql")

func goldenPath(t *testing.T) string {
	return filepath.Join(testutil.ModuleRoot(t), "migrations", "testdata", "schema.golden.sql")
}

func TestSchemaSnapshot(t *testing.T) {
	dsn := testutil.NewEmptyPostgresDSN(t)
	require.NoError(t, newMigrator(t, dsn).Up())
	got := normalizeSchemaDump(testutil.PgDumpSchema(t, dsn))
	if *update {
		require.NoError(t, os.MkdirAll(filepath.Dir(goldenPath(t)), 0o755))
		require.NoError(t, os.WriteFile(goldenPath(t), []byte(got), 0o644))
		return
	}
	want, err := os.ReadFile(goldenPath(t))
	require.NoError(t, err, "run: go test -tags=integration ./migrations -run TestSchemaSnapshot -update")
	require.Equal(t, string(want), got)
}

func TestTableCount24(t *testing.T) {
	dsn := testutil.NewEmptyPostgresDSN(t)
	require.NoError(t, newMigrator(t, dsn).Up())
	tables := queryStrings(t, dsn, `SELECT c.relname FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
		WHERE n.nspname='public' AND c.relkind IN ('r','p') AND NOT c.relispartition AND c.relname <> 'schema_migrations'`)
	require.Len(t, tables, 24, tables)
	legacy := queryStrings(t, dsn, `SELECT relname FROM pg_class WHERE relname IN ('conversation_styles','conversation_style_versions',
		'expression_profiles','expression_profile_versions','character_versions','voice_profiles','voice_provider_configs','character_voice_profiles')`)
	require.Empty(t, legacy)
}

func TestPartitionNormalizationChecks(t *testing.T) {
	dsn := testutil.NewEmptyPostgresDSN(t)
	require.NoError(t, newMigrator(t, dsn).Up())
	for _, parent := range []string{"session_turns", "session_events"} {
		kids := queryStrings(t, dsn, `SELECT c.relname FROM pg_inherits i JOIN pg_class c ON c.oid=i.inhrelid
			JOIN pg_class p ON p.oid=i.inhparent WHERE p.relname='`+parent+`'`)
		require.Len(t, kids, 4, parent)
		def := queryStrings(t, dsn, `SELECT c.relname FROM pg_inherits i JOIN pg_class c ON c.oid=i.inhrelid
			JOIN pg_class p ON p.oid=i.inhparent WHERE p.relname='`+parent+`' AND pg_get_expr(c.relpartbound,c.oid)='DEFAULT'`)
		require.Empty(t, def)
	}
}

func TestFullDownLeavesNothing(t *testing.T) {
	dsn := testutil.NewEmptyPostgresDSN(t)
	m := newMigrator(t, dsn)
	require.NoError(t, m.Up())
	require.NoError(t, m.Down())
	rels := queryStrings(t, dsn, `SELECT c.relname FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
		WHERE n.nspname='public' AND c.relkind IN ('r','p','v','S','i','m','f') AND c.relname NOT LIKE 'schema_migrations%'`)
	require.Empty(t, rels)
	require.Empty(t, queryStrings(t, dsn, `SELECT p.proname FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace
		WHERE n.nspname='public' AND NOT EXISTS (SELECT 1 FROM pg_depend d WHERE d.objid=p.oid AND d.deptype='e')`))
	require.Empty(t, queryStrings(t, dsn, `SELECT tgname FROM pg_trigger WHERE NOT tgisinternal`))
}
