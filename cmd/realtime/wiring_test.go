package main

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/conversation"
	"github.com/nabePi/kotoki-backend/internal/learner"
	"github.com/nabePi/kotoki-backend/internal/memory"
	"github.com/nabePi/kotoki-backend/internal/platform/clock"
	"github.com/nabePi/kotoki-backend/internal/platform/config"
	"github.com/nabePi/kotoki-backend/internal/provider"
	"github.com/nabePi/kotoki-backend/internal/testutil"
)

var (
	_ memory.RetrievalService             = (*memory.Retriever)(nil)
	_ conversation.LearnerContextProvider = (*learner.Service)(nil)
)

// wiringEnv configures a fake-provider realtime process whose connections are never dialled: the
// pool and the Redis client are lazy, so the wiring can be inspected without any server.
func wiringEnv(t *testing.T) (config.Realtime, *pgxpool.Pool, *redis.Client) {
	t.Helper()
	testutil.ApplyEnv(t, testutil.MinimalEnv(t, map[string]string{"PROVIDER_MODE": "fake", "WS_ALLOWED_ORIGINS": "http://localhost:3000"}))
	cfg, err := config.LoadRealtime()
	require.NoError(t, err)
	pool, err := pgxpool.New(context.Background(), "postgres://u:p@127.0.0.1:1/kotoki?sslmode=disable")
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
	t.Cleanup(func() { _ = rdb.Close() })
	return cfg, pool, rdb
}

func TestRealtimeWiring_RouterHasEmbeddingCapability(t *testing.T) {
	_, _, rdb := wiringEnv(t)
	router, _, _, err := buildRouter(rdb, clock.System{}, nil, provider.NewMetrics(nil))
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := router.Embed(ctx, provider.EmbeddingRequest{Texts: []string{"こんにちは"}, RequestID: "wiring"})
	require.NoError(t, err)
	require.Len(t, res.Vectors, 1)
	require.Len(t, res.Vectors[0], 1536, "D-040: the retriever compares 1536-dimensional vectors")
}

func TestRealtimeWiring_UsesRealMemoryAndLearner(t *testing.T) {
	cfg, pool, rdb := wiringEnv(t)
	router, rc, _, err := buildRouter(rdb, clock.System{}, nil, provider.NewMetrics(nil))
	require.NoError(t, err)
	deps := buildConversationDeps(cfg, slog.New(slog.DiscardHandler), pool, rdb, prometheus.NewRegistry(), router, rc)

	require.IsType(t, &memory.Retriever{}, deps.Memory, "the M8 retriever, not the M5 noop")
	require.IsType(t, &learner.Service{}, deps.Learner, "the M8 learner service, not the M5 static one")
	require.NotNil(t, deps.Router)
}

// The fakes of M5 live in testutil/fakes for the M5 tests only: no production file of a binary may
// name them (a noop* port, the static learner, the neutral adaptive provider of M3).
func TestNoFakePortsLeftInProductionWiring(t *testing.T) {
	bad := regexp.MustCompile(`\bnoop[A-Z]\w*|\bstaticLearner\b|\bNeutralAdaptiveProvider\b|\bfakes\.`)
	files, err := filepath.Glob("*.go")
	require.NoError(t, err)
	require.NotEmpty(t, files)
	for _, f := range files {
		if filepath.Ext(f) != ".go" || len(f) > 8 && f[len(f)-8:] == "_test.go" {
			continue
		}
		src, err := os.ReadFile(f)
		require.NoError(t, err)
		require.Empty(t, bad.FindAllString(string(src), -1), f)
	}
}
