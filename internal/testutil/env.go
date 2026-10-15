// Package testutil holds shared test helpers. env.go is the single place that
// lists the environment variables required by every binary.
package testutil

import (
	"testing"
)

// MinimalEnv returns the minimal valid environment for all binaries. Overrides
// are merged in order; an empty value deletes the key.
func MinimalEnv(t testing.TB, overrides ...map[string]string) map[string]string {
	t.Helper()
	env := map[string]string{ //nolint:gosec // G101: throwaway local credentials for tests
		"APP_ENV":   "local",
		"DB_URL":    "postgres://u:p@localhost:5432/kotoki?sslmode=disable",
		"REDIS_URL": "redis://localhost:6379/0",
		// base64 of 32 bytes: a throwaway signing secret for tests
		"AUTH_JWT_SECRET":        "dGVzdC1vbmx5LWp3dC1zZWNyZXQtMDEyMzQ1Njc4OWFi",
		"PRIVACY_POLICY_VERSION": "2026-10-01",
	}
	for _, o := range overrides {
		for k, v := range o {
			if v == "" {
				delete(env, k)
				continue
			}
			env[k] = v
		}
	}
	return env
}

// ApplyEnv sets every key of env via t.Setenv.
func ApplyEnv(t testing.TB, env map[string]string) {
	t.Helper()
	for k, v := range env {
		t.Setenv(k, v)
	}
}
