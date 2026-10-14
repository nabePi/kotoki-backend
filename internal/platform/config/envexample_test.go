package config

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/testutil"
)

func envExample(t *testing.T) map[string]string {
	t.Helper()
	f, err := os.Open(filepath.Join(testutil.ModuleRoot(t), ".env.example"))
	require.NoError(t, err)
	defer func() { _ = f.Close() }()
	out := map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		require.True(t, ok, line)
		out[k] = v
	}
	require.NoError(t, sc.Err())
	return out
}

func TestEnvExampleLoads(t *testing.T) {
	m := envExample(t)
	_, err := loadAPI(m)
	require.NoError(t, err, "api")
	_, err = loadRealtime(m)
	require.NoError(t, err, "realtime")
	_, err = loadWorker(m)
	require.NoError(t, err, "worker")
}

func TestEnvExampleOmitsPerBinaryVars(t *testing.T) {
	m := envExample(t)
	for _, k := range []string{"HTTP_ADDR", "OPS_ADDR", "WORKER_HTTP_ADDR", "WORKER_SHUTDOWN_GRACE", "DB_MAX_CONNS",
		"DB_MIN_CONNS", "REDIS_POOL_SIZE", "SHUTDOWN_TIMEOUT", "DB_STATEMENT_TIMEOUT"} {
		require.NotContains(t, m, k)
	}
}
