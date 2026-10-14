package arch_test

import (
	"os/exec"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMakeTargetsExist(t *testing.T) {
	targets := []string{
		// D-006
		"up", "down", "migrate-up", "migrate-down", "sqlc", "test",
		"test-integration", "lint", "run-api", "run-realtime", "run-worker",
		// §14.10.4
		"env", "up-obs", "reset", "migrate-create", "seed", "promote-admin",
		"sqlc-check", "tidy-check", "migrations-check", "modelname-check",
		"arch-check", "test-cover", "test-e2e", "test-live", "vuln", "build",
		"cli", "docker-build", "load",
	}
	for _, tg := range targets {
		t.Run(tg, func(t *testing.T) {
			cmd := exec.Command("make", "-n", tg)
			cmd.Dir = "../.."
			out, err := cmd.CombinedOutput()
			require.NoError(t, err, string(out))
		})
	}
}
