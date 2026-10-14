// Package clock provides the time port used by feature packages.
package clock

import (
	"sync"
	"time"
)

// Clock returns the current time.
type Clock interface {
	Now() time.Time
}

// System is the real clock; it always returns UTC.
type System struct{}

// Now returns the current time in UTC.
func (System) Now() time.Time { return time.Now().UTC() }

// Fake is a manually driven clock, safe for concurrent use.
type Fake struct {
	mu sync.Mutex
	t  time.Time
}

// NewFake returns a Fake starting at t0 (normalised to UTC).
func NewFake(t0 time.Time) *Fake { return &Fake{t: t0.UTC()} }

// Now returns the fake's current time.
func (f *Fake) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.t
}

// Advance moves the fake clock forward by d.
func (f *Fake) Advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.t = f.t.Add(d)
}

// Set sets the fake clock to t (normalised to UTC).
func (f *Fake) Set(t time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.t = t.UTC()
}
