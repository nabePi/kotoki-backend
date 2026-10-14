package fakes

import (
	"slices"
	"sync"
)

// CallRecorder records repository calls in order, shared by several fakes so a
// test can assert cross-repository sequences (e.g. the lock order of spec 4.6.0).
type CallRecorder struct {
	mu    sync.Mutex
	calls []string
}

// Record appends name; a nil recorder ignores it.
func (r *CallRecorder) Record(name string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, name)
}

// Calls returns the recorded names in order.
func (r *CallRecorder) Calls() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.calls)
}

// Reset forgets all recorded calls.
func (r *CallRecorder) Reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = nil
}
