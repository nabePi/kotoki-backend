//go:build !windows

// Command kotoki-load is the load generator of spec 13.12: `run` plays virtual users against a
// deployment and evaluates the gate, `seed-users` creates the load users.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
)

const usage = `kotoki-load: load generator and gate evaluation (spec 13.12)

usage:
  kotoki-load run        --gate NAME --metrics-urls a,b | --prom-url URL [flags]
  kotoki-load seed-users --count N --admin-email E --admin-password P [flags]

Exit status of run: 0 when every criterion of the gate passed or is skipped, 1 when the gate
failed, 2 on a usage or setup error. Run "kotoki-load <command> -h" for the flags.
`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	rc := realMain(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(rc)
}

func realMain(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		_, _ = fmt.Fprint(stderr, usage)
		return 2
	}
	switch args[0] {
	case "run":
		return cmdRun(ctx, args[1:], stdout, stderr)
	case "seed-users":
		return cmdSeed(ctx, args[1:], stdout, stderr)
	case "-h", "--help", "help":
		_, _ = fmt.Fprint(stdout, usage)
		return 0
	default:
		_, _ = fmt.Fprintf(stderr, "kotoki-load: unknown command %q\n\n%s", args[0], usage)
		return 2
	}
}
