package postgres

import (
	"context"
	"fmt"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/nabePi/kotoki-backend/internal/platform/errs"
)

// partitionLockTimeout bounds how long CREATE TABLE ... PARTITION OF waits for the lock the parent
// table needs (spec 9.9.10): a busy table fails the run instead of stalling inserts behind it.
const partitionLockTimeout = "5s"

// partitionNameRE is the only identifier shape that reaches dynamic DDL; it is checked together
// with the table name against a fixed allow list, and the identifier is quoted as well.
var partitionNameRE = map[string]*regexp.Regexp{
	"session_turns":  regexp.MustCompile(`^session_turns_p(\d{6})$`),
	"session_events": regexp.MustCompile(`^session_events_p(\d{6})$`),
}

// PartitionInfo is one partition as pg_inherits describes it.
type PartitionInfo struct {
	Name          string
	Bound         string // pg_get_expr(relpartbound, oid), e.g. FOR VALUES FROM ('..') TO ('..')
	DetachPending bool   // an interrupted DETACH PARTITION ... CONCURRENTLY awaits FINALIZE
}

// PartitionRepo runs the partition DDL of the partition_maintenance job (spec 9.9.10). DDL is
// built dynamically, so every identifier is validated and quoted; no value comes from a user.
type PartitionRepo struct{ pool *pgxpool.Pool }

// NewPartitionRepo returns the repository over pool. It needs a direct connection (DDL and
// DETACH ... CONCURRENTLY cannot go through a transaction-mode pooler).
func NewPartitionRepo(pool *pgxpool.Pool) *PartitionRepo { return &PartitionRepo{pool: pool} }

func partitionIdent(table, name string) (string, error) {
	re, ok := partitionNameRE[table]
	if !ok {
		return "", fmt.Errorf("postgres: %q is not a partitioned table managed here", table)
	}
	if !re.MatchString(name) {
		return "", fmt.Errorf("postgres: %q is not a partition name of %s", name, table)
	}
	return pgx.Identifier{name}.Sanitize(), nil
}

// List returns every partition of table with its raw bound expression, oldest bound first by name.
func (r *PartitionRepo) List(ctx context.Context, table string) ([]PartitionInfo, error) {
	if _, ok := partitionNameRE[table]; !ok {
		return nil, fmt.Errorf("postgres: %q is not a partitioned table managed here", table)
	}
	rows, err := r.pool.Query(ctx, `
SELECT c.relname, pg_get_expr(c.relpartbound, c.oid), i.inhdetachpending
FROM pg_inherits i
JOIN pg_class c ON c.oid = i.inhrelid
WHERE i.inhparent = $1::regclass
ORDER BY c.relname`, pgx.Identifier{table}.Sanitize())
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeInternalError, "partition list failed")
	}
	defer rows.Close()
	var out []PartitionInfo
	for rows.Next() {
		var p PartitionInfo
		if err := rows.Scan(&p.Name, &p.Bound, &p.DetachPending); err != nil {
			return nil, errs.Wrap(err, errs.CodeInternalError, "partition list scan failed")
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, errs.Wrap(err, errs.CodeInternalError, "partition list failed")
	}
	return out, nil
}

// Ensure creates the partition [lower, upper) of table when it does not exist. lower must be the
// first instant of a UTC month, upper the first instant of the next one, and name must be
// `{table}_pYYYYMM` of that month.
func (r *PartitionRepo) Ensure(ctx context.Context, table, name string, lower, upper time.Time) error {
	ident, err := partitionIdent(table, name)
	if err != nil {
		return err
	}
	lower, upper = lower.UTC(), upper.UTC()
	if !lower.Equal(time.Date(lower.Year(), lower.Month(), 1, 0, 0, 0, 0, time.UTC)) ||
		!upper.Equal(lower.AddDate(0, 1, 0)) || name != table+"_p"+lower.Format("200601") {
		return fmt.Errorf("postgres: partition %s does not match bounds %s..%s", name, lower, upper)
	}
	const layout = "2006-01-02 15:04:05+00"
	stmt := fmt.Sprintf("CREATE TABLE IF NOT EXISTS %s PARTITION OF %s FOR VALUES FROM ('%s') TO ('%s')",
		ident, pgx.Identifier{table}.Sanitize(), lower.Format(layout), upper.Format(layout))
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return errs.Wrap(err, errs.CodeInternalError, "partition begin failed")
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := tx.Exec(ctx, "SET LOCAL lock_timeout = '"+partitionLockTimeout+"'"); err != nil {
		return errs.Wrap(err, errs.CodeInternalError, "partition set lock_timeout failed")
	}
	if _, err := tx.Exec(ctx, stmt); err != nil {
		return errs.Wrap(err, errs.CodeInternalError, "partition create failed")
	}
	if err := tx.Commit(ctx); err != nil {
		return errs.Wrap(err, errs.CodeInternalError, "partition create commit failed")
	}
	return nil
}

// Drop detaches (DETACH ... CONCURRENTLY outside a transaction, or FINALIZE when a previous detach
// was interrupted) and then drops the partition.
func (r *PartitionRepo) Drop(ctx context.Context, table, name string, detachPending bool) error {
	ident, err := partitionIdent(table, name)
	if err != nil {
		return err
	}
	parent := pgx.Identifier{table}.Sanitize()
	mode := "CONCURRENTLY"
	if detachPending {
		mode = "FINALIZE"
	}
	if _, err := r.pool.Exec(ctx, fmt.Sprintf("ALTER TABLE %s DETACH PARTITION %s %s", parent, ident, mode)); err != nil {
		return errs.Wrap(err, errs.CodeInternalError, "partition detach failed")
	}
	if _, err := r.pool.Exec(ctx, "DROP TABLE IF EXISTS "+ident); err != nil {
		return errs.Wrap(err, errs.CodeInternalError, "partition drop failed")
	}
	return nil
}
