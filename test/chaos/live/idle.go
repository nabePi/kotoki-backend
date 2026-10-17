package live

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// IdleWaiter waits until no session is open any more. Two scenarios in a row on one stack share the
// users of the load: the sessions that the first one cut hold those users until the server ends them
// (reconnect timeout, sweeper), and a run that starts meanwhile gets SESSION_ALREADY_OPEN instead of a
// load, so its injection would hit nothing.
type IdleWaiter struct {
	Count   func(ctx context.Context) (int, error)
	Poll    time.Duration // default 2 s
	Timeout time.Duration // default 3 min
	Out     io.Writer
}

// Wait implements the Prepare hook of scenario.Deps.
func (w IdleWaiter) Wait(ctx context.Context) error {
	poll, timeout := w.Poll, w.Timeout
	if poll <= 0 {
		poll = 2 * time.Second
	}
	if timeout <= 0 {
		timeout = 3 * time.Minute
	}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	last := -1
	for {
		n, err := w.Count(ctx)
		if err != nil {
			return fmt.Errorf("count open sessions: %w", err)
		}
		if n == 0 {
			return nil
		}
		if w.Out != nil && n != last {
			_, _ = fmt.Fprintf(w.Out, "waiting for %d session(s) of an earlier run to end\n", n)
		}
		last = n
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("%d session(s) are still open after %s", n, timeout)
		case <-time.After(poll):
		}
	}
}

// CountOpenSessions returns the Count function over the database at dsn: sessions that are not in a
// final status.
func CountOpenSessions(dsn string) func(context.Context) (int, error) {
	return func(ctx context.Context) (int, error) {
		pool, err := pgxpool.New(ctx, dsn)
		if err != nil {
			return 0, errors.New("connect to the database failed")
		}
		defer pool.Close()
		var n int
		err = pool.QueryRow(ctx, `SELECT count(*) FROM sessions WHERE status IN ('RESERVED','STARTING','ACTIVE','ENDING')`).Scan(&n)
		if err != nil {
			return 0, errors.New(strings.ReplaceAll(err.Error(), dsn, "<db>"))
		}
		return n, nil
	}
}
