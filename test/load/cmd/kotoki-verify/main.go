//go:build !windows

// Command kotoki-verify checks the data invariants after a load run (spec 13.12.6), and after a
// restore (restore drill, spec 13.3.4). Exit status: 0 only when there is no violation, 1 on
// violations, 2 when the check could not be done or the flags are wrong.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/nabePi/kotoki-backend/test/load/verify"
)

// runFunc runs the checks against the database at dsn; a fake in tests.
type runFunc func(ctx context.Context, dsn string, o verify.Options) ([]verify.Violation, error)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	rc := realMain(ctx, os.Args[1:], os.Stdout, os.Stderr, runDB)
	stop()
	os.Exit(rc)
}

func runDB(ctx context.Context, dsn string, o verify.Options) ([]verify.Violation, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	defer pool.Close()
	return verify.Run(ctx, pool, o)
}

// scrub keeps the connection string, which may carry a password, out of a message.
func scrub(msg, dsn string) string {
	if dsn == "" {
		return msg
	}
	return strings.ReplaceAll(msg, dsn, "<db>")
}

func realMain(ctx context.Context, args []string, stdout, stderr io.Writer, run runFunc) int {
	fs := flag.NewFlagSet("kotoki-verify", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		dsn, since string
		o          verify.Options
	)
	fs.StringVar(&dsn, "db", envDSN(), "PostgreSQL URL, preferably a read-only role (default: env DATABASE_URL)")
	fs.StringVar(&since, "since", "", "only sessions created at or after this RFC3339 time (V3-V8)")
	fs.DurationVar(&o.AwaitEvaluations, "await-evaluations", 0, "wait this long for the evaluations of completed sessions (V8); 0 leaves V8 out")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	switch {
	case fs.NArg() > 0:
		_, _ = fmt.Fprintf(stderr, "kotoki-verify: unexpected argument %q\n", fs.Arg(0))
		return 2
	case dsn == "":
		_, _ = fmt.Fprintln(stderr, "kotoki-verify: --db (or DATABASE_URL) is required")
		return 2
	case o.AwaitEvaluations < 0:
		_, _ = fmt.Fprintln(stderr, "kotoki-verify: --await-evaluations cannot be negative")
		return 2
	}
	if since != "" {
		t, err := time.Parse(time.RFC3339, since)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "kotoki-verify: --since: %v\n", err)
			return 2
		}
		o.Since = t
	}
	vs, err := run(ctx, dsn, o)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "kotoki-verify: %s\n", scrub(err.Error(), dsn))
		return 2
	}
	for _, v := range vs {
		_, _ = fmt.Fprintf(stdout, "%s %s\n", v.Check, v.Detail)
	}
	word := "violations"
	if len(vs) == 1 {
		word = "violation"
	}
	_, _ = fmt.Fprintf(stdout, "kotoki-verify: %d %s\n", len(vs), word)
	if len(vs) > 0 {
		return 1
	}
	return 0
}

func envDSN() string {
	v, _ := os.LookupEnv("DATABASE_URL")
	return v
}
