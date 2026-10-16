//go:build integration || e2e

package testutil

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ApplySeed runs seed/*.sql in file-name order against pool. Each file is sent as one simple
// protocol Exec (no arguments), like psql -v ON_ERROR_STOP=1; the seed files are idempotent.
func ApplySeed(t testing.TB, pool *pgxpool.Pool) {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(ModuleRoot(t), "seed", "*.sql"))
	if err != nil {
		t.Fatalf("glob seed files: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("no seed/*.sql files found")
	}
	sort.Strings(files)
	for _, f := range files {
		b, err := os.ReadFile(f) //nolint:gosec // seed path under the module root
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		if _, err := pool.Exec(context.Background(), string(b)); err != nil {
			t.Fatalf("apply %s: %v", filepath.Base(f), err)
		}
	}
}
