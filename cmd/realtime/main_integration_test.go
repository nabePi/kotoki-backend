//go:build integration

package main

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pgvector/pgvector-go"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/memory"
	"github.com/nabePi/kotoki-backend/internal/platform/clock"
	"github.com/nabePi/kotoki-backend/internal/platform/config"
	"github.com/nabePi/kotoki-backend/internal/provider"
	"github.com/nabePi/kotoki-backend/internal/testutil"
)

func baseEnv(t *testing.T, addr string, overrides map[string]string) map[string]string {
	t.Helper()
	m := map[string]string{
		"DB_URL": testutil.NewEmptyPostgresDSN(t), "REDIS_URL": "redis://" + testutil.NewRedis(t).Options().Addr,
		"OPS_ADDR": addr, "HTTP_ADDR": testutil.FreeAddr(t), "LOG_FORMAT": "text",
		"PROVIDER_MODE": "fake", // M5 has fake providers only (M9 adds real)
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
	go func() { done <- run(ctx) }()
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
	err := run(ctx)
	require.Error(t, err)
	require.Contains(t, err.Error(), "DB_URL")
	testutil.AssertNothingListening(t, addr)
}

func TestRunRefusesRealModeBeforeListening(t *testing.T) {
	addr := testutil.FreeAddr(t)
	env := baseEnv(t, addr, map[string]string{"PROVIDER_MODE": "real"})
	testutil.ApplyEnv(t, env)
	err := run(context.Background())
	require.Error(t, err)
	require.Contains(t, err.Error(), "M9")
	testutil.AssertNothingListening(t, addr)
	testutil.AssertNothingListening(t, env["HTTP_ADDR"])
}

// The memory port of the runtime is the real retriever over Postgres and Redis, and it embeds the
// scenario query through the pod's router (D-040): an episode whose embedding equals the query's
// comes back, a preference comes back as core, and nothing is degraded.
func TestRealtimeWiring_RetrieverReadsMemoriesThroughRouterEmbedding(t *testing.T) {
	pool := testutil.NewPostgres(t)
	rdb := testutil.NewRedis(t)
	testutil.ApplyEnv(t, testutil.MinimalEnv(t, map[string]string{"PROVIDER_MODE": "fake", "WS_ALLOWED_ORIGINS": "http://localhost:3000"}))
	cfg, err := config.LoadRealtime()
	require.NoError(t, err)
	router, rc, _, err := buildRouter(rdb, clock.System{}, nil, provider.NewMetrics(nil))
	require.NoError(t, err)
	deps := buildConversationDeps(cfg, slog.New(slog.DiscardHandler), pool, rdb, prometheus.NewRegistry(), router, rc)

	ctx := context.Background()
	const query = "面接の練習をします"
	emb, err := router.Embed(ctx, provider.EmbeddingRequest{Texts: []string{query}, RequestID: "wiring-seed"})
	require.NoError(t, err)

	uid := testutil.SeedUser(t, pool, nil)
	now := time.Now().UTC()
	insert := func(category, key, text string, vec *pgvector.Vector) {
		_, err := pool.Exec(ctx, `INSERT INTO memories (id, user_id, category, memory_key, content, display_text, evidence_type, confidence, importance,
			embedding, embedding_model, first_observed_at, last_observed_at, expires_at)
			VALUES ($1, $2, $3, $4, $5, $5, 'EXPLICIT_USER_STATEMENT', 0.95, 0.9, $6, $7, $8, $8, $9)`,
			uuid.Must(uuid.NewV7()), uid, category, key, text, vec, "fake", now, now.Add(24*time.Hour))
		require.NoError(t, err)
	}
	v := pgvector.NewVector(emb.Vectors[0])
	insert("PREFERENCE", "likes:coffee", "コーヒーが好き", nil)
	insert("EPISODIC", "episode:interview", "先週、面接の練習をした", &v)

	mc, err := deps.Memory.BuildSessionContext(ctx, memory.SessionContextInput{UserID: uid, SessionID: uuid.Must(uuid.NewV7()), ScenarioText: query})
	require.NoError(t, err)
	require.False(t, mc.Degraded)
	require.Len(t, mc.Core, 1)
	require.Len(t, mc.Episodic, 1, "the query was embedded through the router")
}
