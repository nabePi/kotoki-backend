package fakes_test

import (
	"errors"
	"testing"

	"github.com/nabePi/kotoki-backend/internal/testutil/fakes"
)

func TestFailInjectorOnce(t *testing.T) {
	var f fakes.FailInjector
	e := errors.New("boom")
	f.FailNext("Get", e)
	if got := f.Take("Get"); !errors.Is(got, e) {
		t.Fatalf("got %v", got)
	}
	if got := f.Take("Get"); got != nil {
		t.Fatalf("second take: %v", got)
	}
	if got := f.Take("Other"); got != nil {
		t.Fatalf("other: %v", got)
	}
}
