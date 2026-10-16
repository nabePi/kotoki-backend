package main

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/nabePi/kotoki-backend/internal/prompt"
)

// RegisterFixtures panics on a second registration of a key, and run/buildAPI can be entered more
// than once per process (tests), so the wiring must be idempotent and complete for M7's keys.
func TestRegisterPromptFixtures_IdempotentAndComplete(t *testing.T) {
	assert.NotPanics(t, func() {
		registerPromptFixtures()
		registerPromptFixtures()
	})
	assert.NotEmpty(t, prompt.Fixtures("EVALUATION"))
	assert.NotEmpty(t, prompt.Fixtures("SUMMARY"))
}
