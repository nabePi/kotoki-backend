//go:build integration || e2e

package testutil

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5" // registers pgx5://
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	tcexec "github.com/testcontainers/testcontainers-go/exec"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

const (
	postgresImage = "pgvector/pgvector:pg17"
	templateDB    = "kotoki_template"
)

var pg struct {
	once      sync.Once
	baseDSN   string // DSN of the maintenance database (no db name substitution needed)
	container testcontainers.Container
	err       error
}

// pgDSN rewrites the database name of dsn.
func pgDSN(dsn, db string) string {
	base, query, _ := strings.Cut(dsn, "?")
	i := strings.LastIndex(base, "/")
	out := base[:i+1] + db
	if query != "" {
		out += "?" + query
	}
	return out
}

func startPostgres(ctx context.Context, root string) (testcontainers.Container, string, error) {
	c, err := tcpostgres.Run(ctx, postgresImage,
		tcpostgres.WithDatabase("postgres"),
		tcpostgres.WithUsername("kotoki"),
		tcpostgres.WithPassword("kotoki"),
		testcontainers.WithCmdArgs("-c", "timezone=UTC", "-c", "fsync=off"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").WithOccurrence(2).WithStartupTimeout(90*time.Second)),
	)
	if err != nil {
		return nil, "", fmt.Errorf("start postgres: %w", err)
	}
	dsn, err := c.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		return nil, "", err
	}
	return c, dsn, migrateTemplate(ctx, dsn, root)
}

func migrateTemplate(ctx context.Context, dsn, root string) error {
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return err
	}
	defer admin.Close()
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+templateDB); err != nil {
		return fmt.Errorf("create template db: %w", err)
	}
	tdsn := pgDSN(dsn, templateDB)
	m, err := migrate.New("file://"+filepath.Join(root, "migrations"), "pgx5://"+strings.TrimPrefix(tdsn, "postgres://"))
	if err != nil {
		return fmt.Errorf("migrator: %w", err)
	}
	defer func() { _, _ = m.Close() }()
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("migrate up: %w", err)
	}
	return nil
}

// maintenanceDSN returns the DSN of the shared container's "postgres" database.
func maintenanceDSN(t testing.TB) string {
	t.Helper()
	pg.once.Do(func() {
		if v := os.Getenv("TEST_DATABASE_URL"); v != "" {
			pg.baseDSN = v
			return
		}
		pg.container, pg.baseDSN, pg.err = startPostgres(context.Background(), ModuleRoot(t))
	})
	if pg.err != nil {
		t.Fatalf("testutil: postgres: %v", pg.err)
	}
	return pg.baseDSN
}

func createDB(t testing.TB, prefix, template string) string {
	t.Helper()
	ctx := context.Background()
	base := maintenanceDSN(t)
	admin, err := pgxpool.New(ctx, pgDSN(base, "postgres"))
	if err != nil {
		t.Fatalf("testutil: admin pool: %v", err)
	}
	name := prefix + strings.ReplaceAll(uuid.NewString(), "-", "")
	stmt := "CREATE DATABASE " + name
	if template != "" {
		stmt += " TEMPLATE " + template
	}
	if _, err := admin.Exec(ctx, stmt); err != nil {
		admin.Close()
		t.Fatalf("testutil: %s: %v", stmt, err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
		admin.Close()
	})
	return pgDSN(base, name)
}

// NewPostgres returns a pool on a fresh, fully migrated database cloned from the
// per-package template. TEST_DATABASE_URL overrides the container (it must point
// at a server whose kotoki_template database is already migrated).
func NewPostgres(t testing.TB) *pgxpool.Pool {
	t.Helper()
	dsn := createDB(t, "t_", templateDB)
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("testutil: pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// NewEmptyPostgresDSN returns the DSN of a new database without any migration.
func NewEmptyPostgresDSN(t testing.TB) string {
	t.Helper()
	return createDB(t, "e_", "template0")
}

// PgDumpSchema returns `pg_dump --schema-only` of the database named in dsn. It
// runs inside the test container, or with the local pg_dump when TEST_DATABASE_URL is set.
func PgDumpSchema(t testing.TB, dsn string) string {
	t.Helper()
	db := dsn[strings.LastIndex(strings.SplitN(dsn, "?", 2)[0], "/")+1:]
	db = strings.SplitN(db, "?", 2)[0]
	args := []string{"pg_dump", "--schema-only", "--no-owner", "--no-privileges", "--no-comments", "-U", "kotoki", "-d", db}
	if pg.container == nil {
		out, err := exec.CommandContext(context.Background(), args[0], append(args[1:len(args)-2], dsn)...).CombinedOutput() //nolint:gosec // test helper, args are fixed
		if err != nil {
			t.Fatalf("testutil: pg_dump: %v\n%s", err, out)
		}
		return string(out)
	}
	code, r, err := pg.container.Exec(context.Background(), args, tcexec.Multiplexed())
	if err != nil {
		t.Fatalf("testutil: pg_dump exec: %v", err)
	}
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("testutil: pg_dump read: %v", err)
	}
	if code != 0 {
		t.Fatalf("testutil: pg_dump exit %d: %s", code, b)
	}
	return string(b)
}
