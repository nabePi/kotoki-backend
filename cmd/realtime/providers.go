package main

import (
	"context"
	"log/slog"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/nabePi/kotoki-backend/cmd/internal/providerwire"
	"github.com/nabePi/kotoki-backend/internal/learner"
	"github.com/nabePi/kotoki-backend/internal/memory"
	"github.com/nabePi/kotoki-backend/internal/platform/clock"
	"github.com/nabePi/kotoki-backend/internal/prompt"
	"github.com/nabePi/kotoki-backend/internal/provider"
	"github.com/nabePi/kotoki-backend/internal/redisstore"
)

// realtimeCaps are the capabilities the realtime router is wired with. EMBEDDING is the async
// capability of the router (fake.NewEmbedding), so that M8 only swaps the memory and learner ports
// for the real ones without touching the provider wiring.
var realtimeCaps = []provider.Capability{provider.CapSTT, provider.CapLLM, provider.CapTTS, provider.CapEmbedding}

// faultRunner polls the fake-provider fault document until its context ends.
type faultRunner interface{ Run(ctx context.Context) }

// buildRouter wires the provider router of the pod. M5 knows only PROVIDER_MODE=fake: the fakes
// read their fault injection from Redis (spec 8.9.6), and the returned runner polls it. Real mode
// is refused here (M9 adds the vendor adapters).
func buildRouter(rdb redis.UniversalClient, clk clock.Clock, logger *slog.Logger, pm *provider.Metrics) (*provider.Router, provider.RouterConfig, faultRunner, error) {
	rc, fc, err := providerwire.ParseConfigs()
	if err != nil {
		return nil, rc, nil, err
	}
	faults := redisstore.NewFakeFaults(rdb, fc.FaultsRedisKey, fc.FaultsPollInterval, clk, logger)
	rd, err := providerwire.BuildFake(rc, fc, realtimeCaps, providerwire.Deps{
		Role: provider.RoleRealtime, Clock: clk, Log: logger, Metrics: pm, Faults: faults,
	})
	if err != nil {
		return nil, rc, nil, err
	}
	router, err := provider.NewRouter(rc, rd)
	if err != nil {
		return nil, rc, nil, err
	}
	return router, rc, faults, nil
}

// noopMemory is the memory port of M5: no memory yet (M8 swaps in memory.Retriever). It never
// fails and reports nothing degraded, so a session runs with an empty memory block.
type noopMemory struct{}

var _ memory.RetrievalService = noopMemory{}

func (noopMemory) BuildSessionContext(_ context.Context, in memory.SessionContextInput) (memory.MemoryContext, error) {
	return memory.MemoryContext{SessionID: in.SessionID}, nil
}

func (noopMemory) RefreshEpisodic(context.Context, uuid.UUID, uuid.UUID, []memory.TurnText) ([]memory.ContextItem, error) {
	return nil, nil
}

func (noopMemory) ReloadCoreIfChanged(context.Context, *memory.MemoryContext) (bool, error) {
	return false, nil
}

// staticLearner is the learner port of M5: an empty LearnerContext (M8 swaps in the real one).
type staticLearner struct{}

func (staticLearner) PromptContext(context.Context, uuid.UUID, learner.AdaptiveDifficulty) (prompt.LearnerContext, error) {
	return prompt.LearnerContext{}, nil
}

func (staticLearner) Rerender(prev prompt.LearnerContext, _ learner.AdaptiveDifficulty) prompt.LearnerContext {
	return prev
}
