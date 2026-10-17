package arch

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/testutil"
)

func readRunScript(t *testing.T) (path, body string) {
	t.Helper()
	path = filepath.Join(testutil.ModuleRoot(t), "test", "load", "local", "run.sh")
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	return path, string(b)
}

// TestRunScript_Syntax: the G1 local script parses, is executable, and passes shellcheck when installed.
func TestRunScript_Syntax(t *testing.T) {
	path, _ := readRunScript(t)
	st, err := os.Stat(path)
	require.NoError(t, err)
	require.NotZero(t, st.Mode().Perm()&0o100, "run.sh must be executable (make load-local checks -x)")

	out, err := exec.Command("bash", "-n", path).CombinedOutput()
	require.NoError(t, err, string(out))

	if sc, err := exec.LookPath("shellcheck"); err == nil {
		out, err := exec.Command(sc, "-x", path).CombinedOutput()
		require.NoError(t, err, string(out))
	}
}

// TestRunScript_Contract pins what makes the local gate honest and safe: the explicit environment of the
// plan, the gate and its checks, a compose project of its own (never the developer's), and no secret on argv.
func TestRunScript_Contract(t *testing.T) {
	_, s := readRunScript(t)

	for _, want := range []string{
		"PROVIDER_MODE=fake", "FAKE_STT_MODE=energy", "WS_ALLOW_EMPTY_ORIGIN=true", "WORKER_HTTP_ADDR=:9092", "ACCOUNT_PURGE_DELAY=1h", "APP_ENV=local",
		"FAKE_PRIMARY_STT_LATENCY_MS=250", "FAKE_PRIMARY_LLM_LATENCY_MS=500", "FAKE_LLM_TOKEN_INTERVAL_MS=30", "FAKE_PRIMARY_TTS_LATENCY_MS=300",
		"--handlers=outbox,evaluation,memory,learner,summary,sweeper,cleanup",
		"--gate \"$GATE\"", "GATE=g1-local", "--metrics-file deploy/k8s/hack/metrics.txt", "--await-evaluations",
		"http://localhost:9090,http://localhost:9091,http://localhost:9092",
		"promote-admin", "seed-users", "--count \"$SEED_COUNT\"", "kotoki-verify", "docs/loadtest/", "-g1-local.md",
		"make \"${MAKE_ARGS[@]}\" migrate-up", "make \"${MAKE_ARGS[@]}\" seed", "make build",
	} {
		require.Contains(t, s, want)
	}
	require.Regexp(t, `SEED_COUNT="\$\{SEED_COUNT:-110\}"`, s, "the plan seeds 110 users")

	// the admin password reaches kotoki-load through the environment only
	require.Contains(t, s, "export KOTOKI_ADMIN_EMAIL=\"$ADMIN_EMAIL\" KOTOKI_ADMIN_PASSWORD")
	require.NotRegexp(t, `--admin-password`, s, "a password flag shows in ps")
	require.NotRegexp(t, `(?m)echo[^\n]*KOTOKI_ADMIN_PASSWORD`, s)
	require.NotRegexp(t, `(?m)curl[^\n]*(-d|--data)[ =]['"]?[^@\n]*PASSWORD`, s, "curl must read the body from stdin")

	// its own compose project; only it is torn down; nothing is force-removed
	require.Contains(t, s, `export COMPOSE_PROJECT_NAME="$PROJECT"`)
	require.Contains(t, s, `PROJECT="${KOTOKI_LOAD_PROJECT:-kotoki-g1local}"`)
	require.Contains(t, s, `"${COMPOSE[@]}" ps -aq`, "refuses to run over an existing project")
	require.Contains(t, s, `"${COMPOSE[@]}" down -v`)
	for _, banned := range []string{"docker rm", "podman rm", "system prune", "volume prune", "docker kill", "rm -rf /"} {
		require.NotContains(t, s, banned)
	}
	for _, line := range strings.Split(s, "\n") {
		if strings.Contains(line, "down -v") && !strings.HasPrefix(strings.TrimSpace(line), "#") && !strings.Contains(line, "docker compose -p") {
			require.Contains(t, line, "${COMPOSE[@]}", "every down -v goes through the project-scoped COMPOSE: %s", line)
		}
	}

	// the compose network follows the project name (the Makefile hard-codes kotoki_default)
	require.Regexp(t, regexp.MustCompile(`COMPOSE_NETWORK=\$\{PROJECT\}_default`), s)

	// a process cleanup that survives failure
	require.Contains(t, s, "trap cleanup EXIT INT TERM")
}
