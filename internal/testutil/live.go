package testutil

import (
	"os"
	"strings"
	"testing"
)

// RequireEnv returns the values of keys, skipping the test when any of them is empty. It is the
// only place a live (`-tags live`) test reads credentials; the skip message names the missing
// variables, never their values.
func RequireEnv(t testing.TB, keys ...string) map[string]string {
	t.Helper()
	out := make(map[string]string, len(keys))
	var missing []string
	for _, k := range keys {
		v := os.Getenv(k)
		if v == "" {
			missing = append(missing, k)
		}
		out[k] = v
	}
	if len(missing) > 0 {
		t.Skipf("live test needs %s", strings.Join(missing, ", "))
	}
	return out
}
