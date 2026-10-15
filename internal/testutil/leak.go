package testutil

import (
	"runtime"
	"strings"
	"testing"
	"time"
)

// NoGoroutineLeak fails the test when, 2 s after it ends, goroutines whose stack mentions
// pkgPrefix (a package path such as "github.com/nabePi/kotoki-backend/internal/conversation")
// exist that were not running when NoGoroutineLeak was called.
func NoGoroutineLeak(t *testing.T, pkgPrefix string) {
	t.Helper()
	before := goroutinesOf(pkgPrefix)
	t.Cleanup(func() {
		if leaked := waitNoNew(before, pkgPrefix, 2*time.Second); len(leaked) > 0 {
			t.Errorf("goroutines of %s leaked:\n%s", pkgPrefix, strings.Join(leaked, "\n\n"))
		}
	})
}

// waitNoNew polls until no goroutine of pkgPrefix outside before is left, and returns the stacks
// still there at the deadline (nil when none).
func waitNoNew(before map[string]string, pkgPrefix string, wait time.Duration) []string {
	var leaked []string
	ok := func() bool {
		leaked = leaked[:0]
		for id, stack := range goroutinesOf(pkgPrefix) {
			if _, existed := before[id]; !existed {
				leaked = append(leaked, stack)
			}
		}
		return len(leaked) == 0
	}
	deadline := time.Now().Add(wait)
	for !ok() {
		if time.Now().After(deadline) {
			return leaked
		}
		time.Sleep(20 * time.Millisecond)
	}
	return nil
}

// goroutinesOf returns the stacks of live goroutines (but the caller's) that mention pkgPrefix,
// keyed by goroutine id.
func goroutinesOf(pkgPrefix string) map[string]string {
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			buf = buf[:n]
			break
		}
		buf = make([]byte, 2*len(buf))
	}
	out := map[string]string{}
	for i, g := range strings.Split(string(buf), "\n\n") {
		if i == 0 || !strings.Contains(g, pkgPrefix) { // the first goroutine is the caller's
			continue
		}
		id, _, _ := strings.Cut(strings.TrimPrefix(g, "goroutine "), " ")
		out[id] = g
	}
	return out
}
