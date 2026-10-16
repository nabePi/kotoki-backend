// Command kotoki-worker is the async process of Kotoki (spec Bab 9). Server mode runs the parts
// the --handlers list selects (outbox publisher, scheduled jobs, queue consumers) behind the ops
// listener; `dlq-replay` and `run-job` are one-shot operator subcommands.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
)

const service = "kotoki-worker"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	err := run(ctx, os.Args[1:])
	stop()
	if err != nil {
		fmt.Fprintln(os.Stderr, service+":", err)
		os.Exit(exitCode(err))
	}
}

// usageError is a wrong command line (unknown job, bad flag): exit status 2, not 1.
type usageError struct{ msg string }

func (e usageError) Error() string { return e.msg }

func usagef(format string, a ...any) error { return usageError{fmt.Sprintf(format, a...)} }

// exitCode maps an error of run to the process exit status.
func exitCode(err error) int {
	var u usageError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &u):
		return 2
	default:
		return 1
	}
}

// run dispatches on args[0]: dlq-replay, run-job, or (anything else) server mode with its flags.
func run(ctx context.Context, args []string) error {
	return runWith(ctx, args, os.Stdout, os.Stderr)
}

// runWith is run with explicit output streams: stdout carries the machine-readable result of a
// subcommand (and the log of server mode), stderr the log of a subcommand.
func runWith(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) > 0 {
		switch args[0] {
		case "dlq-replay":
			return dlqReplayCmd(ctx, args[1:], stdout, stderr)
		case "run-job":
			return runJobCmd(ctx, args[1:], stdout, stderr)
		}
	}
	fs := flag.NewFlagSet(service, flag.ContinueOnError)
	fs.SetOutput(stderr)
	handlersFlag := fs.String("handlers", "", "comma-separated worker handlers (overrides WORKER_HANDLERS)")
	if err := fs.Parse(args); err != nil {
		return usageError{err.Error()}
	}
	return serve(ctx, *handlersFlag, stdout)
}
