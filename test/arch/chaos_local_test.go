package arch

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/testutil"
)

func readChaosScript(t *testing.T) (path, body string) {
	t.Helper()
	path = filepath.Join(testutil.ModuleRoot(t), "test", "chaos", "local", "run.sh")
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	return path, string(b)
}

// TestChaosLocalScript_Syntax: the local chaos script parses, is executable, and passes shellcheck when installed.
func TestChaosLocalScript_Syntax(t *testing.T) {
	path, _ := readChaosScript(t)
	st, err := os.Stat(path)
	require.NoError(t, err)
	require.NotZero(t, st.Mode().Perm()&0o100, "run.sh must be executable (make chaos-local checks -x)")
	out, err := exec.Command("bash", "-n", path).CombinedOutput()
	require.NoError(t, err, string(out))
	if sc, err := exec.LookPath("shellcheck"); err == nil {
		out, err := exec.Command(sc, "-x", path).CombinedOutput()
		require.NoError(t, err, string(out))
	}
}

// TestChaosLocalScript_Contract pins what makes the local chaos run safe: a compose project of its own
// that is the only one torn down, the explicit fake environment, scenarios selected by id, no secret on argv.
func TestChaosLocalScript_Contract(t *testing.T) {
	_, s := readChaosScript(t)
	for _, want := range []string{
		"PROVIDER_MODE=fake", "FAKE_STT_MODE=energy", "WS_ALLOW_EMPTY_ORIGIN=true", "APP_ENV=local", "FAKE_FAULTS_POLL_INTERVAL=1s",
		"--handlers=outbox,evaluation,memory,learner,summary,sweeper,cleanup",
		"bin/kotoki-chaos run \"$id\" --env compose --compose-project \"$PROJECT\"",
		"--metrics-urls http://localhost:9090,http://localhost:9091,http://localhost:9092",
		"seed-users", "promote-admin", "make \"${MAKE_ARGS[@]}\" migrate-up",
	} {
		require.Contains(t, s, want)
	}
	require.Contains(t, s, `export KOTOKI_ADMIN_EMAIL="$ADMIN_EMAIL" KOTOKI_ADMIN_PASSWORD`)
	require.NotRegexp(t, `--admin-password`, s)
	require.NotRegexp(t, `(?m)echo[^\n]*KOTOKI_ADMIN_PASSWORD`, s)

	require.Contains(t, s, `export COMPOSE_PROJECT_NAME="$PROJECT"`)
	require.Contains(t, s, `PROJECT="${KOTOKI_CHAOS_PROJECT:-kotoki-chaoslocal}"`)
	require.NotContains(t, s, `"kotoki"`+"\n", "never the developer's own project")
	require.Contains(t, s, `"${COMPOSE[@]}" ps -aq`, "refuses to run over an existing project")
	for _, banned := range []string{"docker rm", "podman rm", "system prune", "volume prune", "docker kill", "rm -rf /"} {
		require.NotContains(t, s, banned)
	}
	for _, line := range strings.Split(s, "\n") {
		if strings.Contains(line, "down -v") && !strings.HasPrefix(strings.TrimSpace(line), "#") && !strings.Contains(line, "docker compose -p") {
			require.Contains(t, line, "${COMPOSE[@]}", "every down -v goes through the project-scoped COMPOSE: %s", line)
		}
	}
	require.Contains(t, s, "COMPOSE_NETWORK=${PROJECT}_default")
	require.Contains(t, s, "trap cleanup EXIT INT TERM")
}
