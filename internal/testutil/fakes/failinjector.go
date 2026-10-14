// Package fakes holds reusable test doubles.
package fakes

import "sync"

// FailInjector arms one-shot errors per method name. Safe for concurrent use.
type FailInjector struct {
	mu   sync.Mutex
	errs map[string]error
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

// Take returns and clears the armed error for method, or nil.
func (f *FailInjector) Take(method string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	err := f.errs[method]
	delete(f.errs, method)
	return err
}
