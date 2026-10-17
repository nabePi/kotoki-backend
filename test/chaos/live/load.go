package live

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/nabePi/kotoki-backend/test/load/verify"
)

// Loader runs the kotoki-load binary as the load of a chaos scenario: `run` with no --hold, so it
// plays until it is interrupted (SIGINT), then evaluates its gate and exits.
type Loader struct {
	Bin  string
	Args []string // everything but --vus
	Env  []string // nil: the environment of this process
	Out  io.Writer
	// StartGrace is how long the process must stay alive after the start for the start to count
	// (a bad flag or an unreachable server ends it at once): default 3 s. StopTimeout is how long
	// it gets after SIGINT before it is killed: default 90 s.
	StartGrace, StopTimeout time.Duration
}

// Start implements scenario.Loader.
func (l Loader) Start(ctx context.Context, vus int) (func() error, error) {
	grace, stopTimeout := l.StartGrace, l.StopTimeout
	if grace <= 0 {
		grace = 3 * time.Second
	}
	if stopTimeout <= 0 {
		stopTimeout = 90 * time.Second
	}
	args := append(append([]string(nil), l.Args...), "--vus", strconv.Itoa(vus))
	// not bound to ctx: the load is stopped with SIGINT so that it can write its result
	cmd := exec.CommandContext(context.WithoutCancel(ctx), l.Bin, args...) //nolint:gosec // G204: the operator chooses the load binary
	cmd.Env = l.Env
	cmd.Stdout, cmd.Stderr = l.Out, l.Out
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", l.Bin, err)
	}
	exited := make(chan struct{})
	var waitErr error // written before exited is closed
	go func() { waitErr = cmd.Wait(); close(exited) }()

	select {
	case <-exited:
		return nil, fmt.Errorf("load exited right after the start: %w", exitOr(waitErr))
	case <-time.After(grace):
	case <-ctx.Done():
		_ = cmd.Process.Signal(syscall.SIGINT)
		<-exited
		return nil, ctx.Err()
	}

	// a cancelled run interrupts the load as well, so that it is never left behind
	go func() {
		select {
		case <-ctx.Done():
			_ = cmd.Process.Signal(syscall.SIGINT)
		case <-exited:
		}
	}()
	var (
		once    sync.Once
		stopErr error
	)
	return func() error {
		once.Do(func() {
			_ = cmd.Process.Signal(syscall.SIGINT)
			select {
			case <-exited:
			case <-time.After(stopTimeout):
				_ = cmd.Process.Kill()
				<-exited
				stopErr = errors.New("kotoki-load did not stop after SIGINT and was killed")
				return
			}
			var ee *exec.ExitError
			switch {
			case waitErr == nil:
			case errors.As(waitErr, &ee) && ee.ExitCode() == 1:
				// exit 1 is the load's own gate failing, which is what injection is expected to do
			default:
				stopErr = fmt.Errorf("kotoki-load: %w", waitErr)
			}
		})
		return stopErr
	}, nil
}

func exitOr(err error) error {
	if err == nil {
		return errors.New("exit status 0")
	}
	return err
}

// Verifier runs the checks of kotoki-verify (V1-V8) in this process.
type Verifier struct {
	DSN   string
	Await time.Duration // V8: how long completed sessions get to receive their evaluation
	Out   io.Writer
	// Check runs the checks; verify.Run over a pgx pool when nil (see RunDB).
	Check func(ctx context.Context, dsn string, o verify.Options) ([]verify.Violation, error)
}

// Run implements scenario.Verifier: it returns the number of violations.
func (v Verifier) Run(ctx context.Context, since time.Time) (int, error) {
	if v.DSN == "" {
		return 0, errors.New("verify: no database URL")
	}
	check := v.Check
	if check == nil {
		check = RunDB
	}
	vs, err := check(ctx, v.DSN, verify.Options{Since: since, AwaitEvaluations: v.Await})
	if err != nil {
		return 0, errors.New(strings.ReplaceAll(err.Error(), v.DSN, "<db>"))
	}
	if v.Out != nil {
		for _, x := range vs {
			_, _ = fmt.Fprintf(v.Out, "VIOLATION %s %s\n", x.Check, x.Detail)
		}
	}
	return len(vs), nil
}
