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

// seedFixture builds a seed dir (0010, 0020, env/staging/0900) and a stub psql that appends its
// arguments to a log and fails when STUB_FAIL_ON occurs in them. It returns dir, PATH and log.
func seedFixture(t *testing.T) (dir, path, logFile string) {
	t.Helper()
	tmp := t.TempDir()
	dir = filepath.Join(tmp, "seed")
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "env", "staging"), 0o755))
	for _, f := range []string{"0020_b.sql", "0010_a.sql", "env/staging/0900_v.sql"} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, f), []byte("SELECT 1;\n"), 0o644))
	}
	bin := filepath.Join(tmp, "bin")
	require.NoError(t, os.MkdirAll(bin, 0o755))
	logFile = filepath.Join(tmp, "psql.log")
	stub := "#!/bin/sh\necho \"$*\" >> \"$PSQL_LOG\"\ncase \"$*\" in *\"${STUB_FAIL_ON:-@@none@@}\"*) exit 1;; esac\nexit 0\n"
	require.NoError(t, os.WriteFile(filepath.Join(bin, "psql"), []byte(stub), 0o755))
	return dir, bin + string(os.PathListSeparator) + os.Getenv("PATH"), logFile
}

func runSeed(t *testing.T, dir, path, logFile, env string, extra ...string) ([]string, error) {
	t.Helper()
	cmd := exec.Command("sh", filepath.Join(testutil.ModuleRoot(t), "deploy/docker/seed.sh"))
	cmd.Env = append([]string{"PATH=" + path, "PSQL_LOG=" + logFile, "SEED_DIR=" + dir,
		"DB_MIGRATE_URL=postgres://u:p@h/db", "KOTOKI_ENV=" + env}, extra...)
	out, err := cmd.CombinedOutput()
	var lines []string
	if b, rerr := os.ReadFile(logFile); rerr == nil {
		lines = strings.Split(strings.TrimSpace(string(b)), "\n")
	}
	if err != nil {
		t.Logf("seed.sh output: %s", out)
	}
	return lines, err
}

func TestSeedScript_OrderAndEnvDir(t *testing.T) {
	t.Run("staging runs common then env dir in name order", func(t *testing.T) {
		dir, path, logFile := seedFixture(t)
		lines, err := runSeed(t, dir, path, logFile, "staging")
		require.NoError(t, err)
		require.Len(t, lines, 3)
		for i, want := range []string{"0010_a.sql", "0020_b.sql", "env/staging/0900_v.sql"} {
			require.Contains(t, lines[i], "-f "+filepath.Join(dir, want), "line %d", i)
			require.Contains(t, lines[i], "-v ON_ERROR_STOP=1")
			require.Contains(t, lines[i], "postgres://u:p@h/db")
		}
	})
	t.Run("missing env dir runs only the common files", func(t *testing.T) {
		dir, path, logFile := seedFixture(t)
		lines, err := runSeed(t, dir, path, logFile, "prod")
		require.NoError(t, err)
		require.Len(t, lines, 2)
		require.Contains(t, lines[0], "0010_a.sql")
		require.Contains(t, lines[1], "0020_b.sql")
	})
	t.Run("psql failure stops the script with a non-zero status", func(t *testing.T) {
		dir, path, logFile := seedFixture(t)
		lines, err := runSeed(t, dir, path, logFile, "staging", "STUB_FAIL_ON=0020_b")
		require.Error(t, err)
		require.Len(t, lines, 2, "0900 must not run after a failure")
		require.NotContains(t, strings.Join(lines, "\n"), "0900_v")
	})
	t.Run("missing DB_MIGRATE_URL or KOTOKI_ENV is refused before psql runs", func(t *testing.T) {
		dir, path, logFile := seedFixture(t)
		cmd := exec.Command("sh", filepath.Join(testutil.ModuleRoot(t), "deploy/docker/seed.sh"))
		cmd.Env = []string{"PATH=" + path, "PSQL_LOG=" + logFile, "SEED_DIR=" + dir, "KOTOKI_ENV=staging"}
		require.Error(t, cmd.Run())
		cmd = exec.Command("sh", filepath.Join(testutil.ModuleRoot(t), "deploy/docker/seed.sh"))
		cmd.Env = []string{"PATH=" + path, "PSQL_LOG=" + logFile, "SEED_DIR=" + dir, "DB_MIGRATE_URL=postgres://x"}
		require.Error(t, cmd.Run())
		_, statErr := os.Stat(logFile)
		require.True(t, os.IsNotExist(statErr), "psql must not have run")
	})
}

func TestSeedDockerfile(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(testutil.ModuleRoot(t), "deploy/docker/seed.Dockerfile"))
	require.NoError(t, err)
	s := string(b)
	require.Regexp(t, `(?m)^FROM postgres:17-alpine\s*$`, s)
	require.Regexp(t, `(?m)^USER 65532(:65532)?\s*$`, s)
	require.Regexp(t, `(?m)^COPY seed /seed\s*$`, s)
	require.Regexp(t, `(?m)^ENTRYPOINT \["/usr/local/bin/seed\.sh"\]\s*$`, s)
	require.Contains(t, s, "seed.sh /usr/local/bin/seed.sh")
}
