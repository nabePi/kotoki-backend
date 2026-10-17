//go:build !windows

package main

import (
	"flag"
	"fmt"
	"io"

	"github.com/nabePi/kotoki-backend/test/load/capacity"
)

// cmdCapacity prints the capacity model of spec 13.11 for --ccu sessions and, with --measured,
// compares the measurements of a load run with it (exit 1 when one is outside the model).
func cmdCapacity(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("capacity", flag.ContinueOnError)
	fs.SetOutput(stderr)
	ccu := fs.Int("ccu", 0, "concurrent sessions of the model (required, > 0)")
	measured := fs.String("measured", "", "JSON file with the measured values (keys: cpu_cores_per_100_sessions, mem_mb_per_100_sessions, stt_egress_mbps, pg_tx_per_sec, redis_ops_per_sec)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		_, _ = fmt.Fprintf(stderr, "kotoki-load capacity: unexpected argument %q\n", fs.Arg(0))
		return 2
	}
	if *ccu <= 0 {
		_, _ = fmt.Fprintln(stderr, "kotoki-load capacity: --ccu must be > 0")
		return 2
	}
	d := capacity.Default(*ccu).Derive()
	if *measured == "" {
		_, _ = fmt.Fprint(stdout, capacity.RenderDerived(*ccu, d))
		return 0
	}
	m, err := capacity.LoadMeasured(*measured)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "kotoki-load capacity:", err)
		return 2
	}
	fs2 := capacity.Validate(d, m)
	_, _ = fmt.Fprint(stdout, capacity.RenderDerived(*ccu, d), "\nmeasured vs model\n", capacity.RenderFindings(fs2))
	if !capacity.AllOK(fs2) {
		return 1
	}
	return 0
}
