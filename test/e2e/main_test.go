//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"testing"
)

// TestMain starts the whole stack once (spec 14.5): Postgres and Redis containers, migrations and
// seed, then the real cmd/api and cmd/realtime binaries as subprocesses. A failing run prints the
// subprocess logs.
func TestMain(m *testing.M) { os.Exit(run(m)) }

func run(m *testing.M) (code int) {
	tb := &mainTB{}
	defer tb.runCleanups()
	defer func() {
		if r := recover(); r != nil {
			if _, ok := r.(mainFatal); !ok {
				panic(r)
			}
			fmt.Fprintln(os.Stderr, "e2e: setup failed:", string(r.(mainFatal)))
			stack.dumpLogs(os.Stderr)
			code = 1
		}
	}()
	stack = setup(tb)
	tb.cleanup(stack.stop)
	code = m.Run()
	if code != 0 {
		stack.dumpLogs(os.Stderr)
	}
	return code
}

// mainFatal is what mainTB.Fatalf panics with; run recovers it.
type mainFatal string

// mainTB lets the testutil helpers (which take a testing.TB) run from TestMain, where there is no
// *testing.T. Only the methods the helpers use are implemented; the embedded nil interface makes
// any other call panic, which is the intent.
type mainTB struct {
	testing.TB
	cleanups []func()
}

func (b *mainTB) Helper() {}

func (b *mainTB) Fatalf(format string, args ...any) { panic(mainFatal(fmt.Sprintf(format, args...))) }

func (b *mainTB) Logf(format string, args ...any) { fmt.Fprintf(os.Stderr, format+"\n", args...) }

func (b *mainTB) Cleanup(f func()) { b.cleanups = append(b.cleanups, f) }

func (b *mainTB) cleanup(f func()) { b.Cleanup(f) }

// runCleanups runs the cleanups in reverse order, like testing.
func (b *mainTB) runCleanups() {
	for i := len(b.cleanups) - 1; i >= 0; i-- {
		b.cleanups[i]()
	}
	b.cleanups = nil
}
