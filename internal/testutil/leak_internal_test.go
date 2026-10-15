package testutil

import (
	"strings"
	"testing"
	"time"
)

func parkForLeakTest(release <-chan struct{}) { <-release }

func TestLeakReportNamesTheGoroutine(t *testing.T) {
	const pkg = "github.com/nabePi/kotoki-backend/internal/testutil"
	before := goroutinesOf(pkg)
	release := make(chan struct{})
	go parkForLeakTest(release)
	leaked := waitNoNew(before, pkg, 50*time.Millisecond)
	close(release)
	if len(leaked) != 1 || !strings.Contains(leaked[0], "parkForLeakTest") {
		t.Fatalf("the report must carry the leaked stack, got %q", leaked)
	}
	if left := waitNoNew(before, pkg, 2*time.Second); left != nil {
		t.Fatalf("goroutine still running after release: %q", left)
	}
}
