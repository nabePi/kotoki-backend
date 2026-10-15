//go:build integration

package main

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/testutil"
)

func baseEnv(t *testing.T, addr string, overrides map[string]string) map[string]string {
	t.Helper()
	m := map[string]string{
		"DB_URL": testutil.NewEmptyPostgresDSN(t), "REDIS_URL": "redis://" + testutil.NewRedis(t).Options().Addr,
		"WORKER_HTTP_ADDR": addr, "LOG_FORMAT": "text",
	}
	for k, v := range overrides {
		m[k] = v
	}
	return testutil.MinimalEnv(t, m)
}

func TestRunServesOpsAndStopsCleanly(t *testing.T) {
	addr := testutil.FreeAddr(t)
	testutil.ApplyEnv(t, baseEnv(t, addr, nil))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- run(ctx, []string{"--handlers=outbox,evaluation"}) }()
	testutil.AssertOpsEndpoints(t, addr, done)
	cancel()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("run did not return within 5s of cancel")
	}
}

func TestRunFailsFastOnBadConfig(t *testing.T) {
	addr := testutil.FreeAddr(t)
	env := baseEnv(t, addr, map[string]string{"DB_URL": ""})
	testutil.ApplyEnv(t, env)
	t.Setenv("DB_URL", "")
	ctx := context.Background()
	err := run(ctx, []string{"--handlers=outbox,evaluation"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "DB_URL")
	testutil.AssertNothingListening(t, addr)
}

func TestWorkerRejectsUnknownHandler(t *testing.T) {
	testutil.ApplyEnv(t, baseEnv(t, testutil.FreeAddr(t), nil))
	err := run(context.Background(), []string{"--handlers=outbox,bogus"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "bogus")
}

func TestWorkerRequiresHandlers(t *testing.T) {
	testutil.ApplyEnv(t, baseEnv(t, testutil.FreeAddr(t), nil))
	t.Setenv("WORKER_HANDLERS", "")
	require.Error(t, run(context.Background(), nil))
}
