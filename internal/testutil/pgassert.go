//go:build integration || e2e

package testutil

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// DBTX is the subset of pgx implemented by pools, conns and transactions.
type DBTX interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Row is a column->value map used by TryInsert/MustInsert.
type Row map[string]any

// RequirePgErr asserts err is a *pgconn.PgError with the given SQLSTATE and,
// when constraint is non-empty, constraint name.
func RequirePgErr(t testing.TB, err error, sqlstate, constraint string) {
	t.Helper()
	var pe *pgconn.PgError
	if !errors.As(err, &pe) {
		t.Fatalf("testutil: expected *pgconn.PgError, got %T: %v", err, err)
	}
	if pe.Code != sqlstate {
		t.Fatalf("testutil: sqlstate = %s (%s), want %s", pe.Code, pe.Message, sqlstate)
	}
	if constraint != "" && pe.ConstraintName != constraint {
		t.Fatalf("testutil: constraint = %q, want %q", pe.ConstraintName, constraint)
	}
}

// TryInsert inserts r into table and returns the error unmodified.
func TryInsert(ctx context.Context, db DBTX, table string, r Row) error {
	cols := make([]string, 0, len(r))
	for c := range r {
		cols = append(cols, c)
	}
	sort.Strings(cols)
	args := make([]any, len(cols))
	ph := make([]string, len(cols))
	for i, c := range cols {
		args[i] = r[c]
		ph[i] = fmt.Sprintf("$%d", i+1)
	}
	q := fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s)", table, strings.Join(cols, ", "), strings.Join(ph, ", "))
	_, err := db.Exec(ctx, q, args...)
	return err
}

// MustInsert is TryInsert that fails the test on error.
func MustInsert(t testing.TB, ctx context.Context, db DBTX, table string, r Row) {
	t.Helper()
	if err := TryInsert(ctx, db, table, r); err != nil {
		t.Fatalf("testutil: insert into %s: %v", table, err)
	}
}
