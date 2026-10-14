package prompt

import (
	"fmt"
	"sync"
)

var (
	fixtureMu sync.Mutex
	fixtures  = map[string]func() []any{}
	// builtinFixtures re-register the fixtures owned by this package (CONVERSATION, OPENING)
	// after ResetFixturesForTest.
	builtinFixtures []func()
)

// RegisterFixtures lets the package that owns a prompt key (Bab 9–12) register the data values
// the publish validator renders its template against. Registering a key twice panics: two
// owners for one key is a wiring bug.
func RegisterFixtures(key string, fn func() []any) {
	fixtureMu.Lock()
	defer fixtureMu.Unlock()
	if _, dup := fixtures[key]; dup {
		panic(fmt.Sprintf("prompt: fixtures for key %q are already registered", key))
	}
	fixtures[key] = fn
}

// Fixtures returns the registered fixture values of key, or nil.
func Fixtures(key string) []any {
	fixtureMu.Lock()
	fn := fixtures[key]
	fixtureMu.Unlock()
	if fn == nil {
		return nil
	}
	return fn()
}

// ResetFixturesForTest restores the registry to the fixtures this package owns; tests only.
func ResetFixturesForTest() {
	fixtureMu.Lock()
	fixtures = map[string]func() []any{}
	builtin := builtinFixtures
	fixtureMu.Unlock()
	for _, f := range builtin {
		f()
	}
}
