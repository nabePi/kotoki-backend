package main

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/content"
	"github.com/nabePi/kotoki-backend/internal/learner"
	"github.com/nabePi/kotoki-backend/internal/memory"
	"github.com/nabePi/kotoki-backend/internal/platform/clock"
	"github.com/nabePi/kotoki-backend/internal/platform/idgen"
	"github.com/nabePi/kotoki-backend/internal/postgres"
	"github.com/nabePi/kotoki-backend/internal/prompt"
	"github.com/nabePi/kotoki-backend/internal/session"
)

func lazyServices(t *testing.T) profileServices {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), "postgres://u:p@127.0.0.1:1/kotoki?sslmode=disable")
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
	t.Cleanup(func() { _ = rdb.Close() })
	ids, clk := idgen.NewV7(), clock.System{}
	store := postgres.NewStore(pool, postgres.WithIDs(ids), postgres.WithClock(clk))
	return buildProfileServices(store, pool, rdb, content.NewSessionConfigLoader(postgres.NewContentRepo(pool)), clk, ids, slog.New(slog.DiscardHandler))
}

func TestAPIWiring_AdaptiveIsLearnerBacked(t *testing.T) {
	ps := lazyServices(t)
	require.IsType(t, &adaptiveAdapter{}, ps.adaptive, "session.Deps.Adaptive is the learner-backed adapter")
	require.NotEqual(t, session.AdaptiveProvider(session.NeutralAdaptiveProvider{}), ps.adaptive)
	require.NotNil(t, ps.learner)
	require.NotNil(t, ps.memory)
	require.Same(t, ps.learner, ps.adaptive.(*adaptiveAdapter).svc, "the adapter and /profile share one learner service")
}

func TestAPIWiring_ProfileAndMemoryServicesSatisfyHTTPPorts(t *testing.T) {
	ps := lazyServices(t)
	require.IsType(t, &learner.Service{}, ps.learner)
	require.IsType(t, &memory.Service{}, ps.memory)
}

func TestPublishValidatorKnowsMemoryExtractionFixtures(t *testing.T) {
	registerPromptFixtures()
	require.GreaterOrEqual(t, len(prompt.Fixtures("MEMORY_EXTRACTION")), 2)
}

// No production file may fall back to the neutral adaptive provider of M3 (or any noop port).
func TestNoFakePortsLeftInProductionWiring(t *testing.T) {
	bad := regexp.MustCompile(`\bnoop[A-Z]\w*|\bstaticLearner\b|\bNeutralAdaptiveProvider\b|\bfakes\.`)
	files, err := filepath.Glob("*.go")
	require.NoError(t, err)
	require.NotEmpty(t, files)
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		require.NoError(t, err)
		require.Empty(t, bad.FindAllString(string(src), -1), f)
	}
}
