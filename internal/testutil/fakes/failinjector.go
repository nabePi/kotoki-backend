// Package fakes holds reusable test doubles.
package fakes

import "sync"

// FailInjector arms one-shot errors per method name. Safe for concurrent use.
type FailInjector struct {
	mu     sync.Mutex
	errs   map[string]error
	always error
}

// FailNext arms err for the next Take(method).
func (f *FailInjector) FailNext(method string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.errs == nil {
		f.errs = map[string]error{}
	}
	f.errs[method] = err
}

// FailAlways makes every Take fail with err until it is called again with nil (a store that is
// down, as opposed to FailNext's one bad call). An error armed with FailNext goes first.
func (f *FailInjector) FailAlways(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.always = err
}

// Take returns and clears the armed error for method, or the standing one, or nil.
func (f *FailInjector) Take(method string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err, ok := f.errs[method]; ok {
		delete(f.errs, method)
		return err
	}
	return f.always
}
