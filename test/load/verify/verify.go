//go:build !windows

// Package verify checks the data invariants a load run (or a restored backup) must leave intact,
// spec 13.12.6. Every check is read-only and returns the offending rows as Violations.
package verify

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Violation is one offending row of a check.
type Violation struct {
	Check  string
	Detail string
}

// Options tune a Run.
type Options struct {
	// Since limits V3-V8 to sessions created at or after it (zero: all). V1 and V2 always look at
	// every account.
	Since time.Time
	// AwaitEvaluations turns V8 on: completed sessions get that long to receive a COMPLETED
	// evaluation (K13). Zero leaves V8 out, so its absence can never read as success.
	AwaitEvaluations time.Duration
	// PollEvery is how often V8 looks again while waiting (default 5 s).
	PollEvery time.Duration

	probe func(ctx context.Context, tx pgx.Tx) // tests only: runs inside each check's transaction
}

const (
	statementTimeout = "60s"
	defaultPoll      = 5 * time.Second
)

// Run executes the checks and returns every violation (an empty, non-nil slice when clean). Each
// check runs in its own READ ONLY transaction with statement_timeout 60 s. V8 waits up to
// AwaitEvaluations for the asynchronous evaluations and returns early when none is missing.
func Run(ctx context.Context, db *pgxpool.Pool, o Options) ([]Violation, error) {
	out := []Violation{}
	for _, c := range checks {
		if c.ID == "V8" {
			continue
		}
		vs, err := runCheck(ctx, db, c, o)
		if err != nil {
			return nil, err
		}
		out = append(out, vs...)
	}
	if o.AwaitEvaluations > 0 {
		vs, err := awaitV8(ctx, db, o)
		if err != nil {
			return nil, err
		}
		out = append(out, vs...)
	}
	return out, nil
}

func awaitV8(ctx context.Context, db *pgxpool.Pool, o Options) ([]Violation, error) {
	c := checks[len(checks)-1]
	poll := o.PollEvery
	if poll <= 0 {
		poll = defaultPoll
	}
	deadline := time.NewTimer(o.AwaitEvaluations)
	defer deadline.Stop()
	for {
		vs, err := runCheck(ctx, db, c, o)
		if err != nil || len(vs) == 0 {
			return nil, err
		}
		wait := time.NewTimer(poll)
		select {
		case <-ctx.Done():
			wait.Stop()
			return nil, ctx.Err()
		case <-deadline.C:
			wait.Stop()
			return runCheck(ctx, db, c, o) // the last look, after the whole window
		case <-wait.C:
		}
	}
}

func runCheck(ctx context.Context, db *pgxpool.Pool, c check, o Options) ([]Violation, error) {
	tx, err := db.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, fmt.Errorf("verify %s: begin: %w", c.ID, err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := tx.Exec(ctx, "SET LOCAL statement_timeout = '"+statementTimeout+"'"); err != nil {
		return nil, fmt.Errorf("verify %s: set timeout: %w", c.ID, err)
	}
	if o.probe != nil {
		o.probe(ctx, tx)
	}
	var args []any
	if c.Since {
		args = append(args, o.Since)
	}
	rows, err := tx.Query(ctx, c.SQL, args...)
	if err != nil {
		return nil, fmt.Errorf("verify %s: %w", c.ID, err)
	}
	defer rows.Close()
	var out []Violation
	for rows.Next() {
		var detail string
		if err := rows.Scan(&detail); err != nil {
			return nil, fmt.Errorf("verify %s: %w", c.ID, err)
		}
		out = append(out, Violation{Check: c.ID, Detail: detail})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("verify %s: %w", c.ID, err)
	}
	return out, nil
}
