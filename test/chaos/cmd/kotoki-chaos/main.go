//go:build !windows

// Command kotoki-chaos plays one failure-injection scenario of spec 13.13: it starts kotoki-load,
// injects the fault, reverts it, runs kotoki-verify and checks the assertions of the scenario.
//
//	kotoki-chaos list
//	kotoki-chaos run F1 --env compose --compose-project kotoki-chaoslocal --metrics-urls ... --db ...
//
// Exit status of run: 0 when the scenario passed, 1 when it ran and did not pass (a failed
// assertion, or one that needs a person), 2 when it could not be run.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
)

const usage = `kotoki-chaos: failure injection F1-F16 (spec 13.13)

usage:
  kotoki-chaos list
  kotoki-chaos run <F1..F16> --env compose|kube [flags]

Run "kotoki-chaos run -h" for the flags.
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
	case "list":
		return cmdList(stdout)
	case "run":
		return cmdRun(ctx, args[1:], stdout, stderr)
	case "-h", "--help", "help":
		_, _ = fmt.Fprint(stdout, usage)
		return 0
	}
	_, _ = fmt.Fprintf(stderr, "kotoki-chaos: unknown command %q\n\n%s", args[0], usage)
	return 2
}
