package testutil_test

import (
	"testing"

	"github.com/nabePi/kotoki-backend/internal/testutil"
)

func TestNoGoroutineLeakPasses(t *testing.T) {
	testutil.NoGoroutineLeak(t, "github.com/nabePi/kotoki-backend/internal/testutil_test")
	done := make(chan struct{})
	go func() { <-done }()
	close(done)
}
