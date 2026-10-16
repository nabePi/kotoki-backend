package main

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/nabePi/kotoki-backend/cmd/internal/providerwire"
	"github.com/nabePi/kotoki-backend/internal/content"
	"github.com/nabePi/kotoki-backend/internal/learner"
	"github.com/nabePi/kotoki-backend/internal/memory"
	"github.com/nabePi/kotoki-backend/internal/platform/clock"
	"github.com/nabePi/kotoki-backend/internal/platform/idgen"
	"github.com/nabePi/kotoki-backend/internal/postgres"
	"github.com/nabePi/kotoki-backend/internal/provider"
	"github.com/nabePi/kotoki-backend/internal/redisstore"
)

// realtimeCaps are the capabilities the realtime router is wired with. EMBEDDING is the async
// capability of the router (fake.NewEmbedding) that the memory retriever embeds its query with.
var realtimeCaps = []provider.Capability{provider.CapSTT, provider.CapLLM, provider.CapTTS, provider.CapEmbedding}

// faultRunner polls the fake-provider fault document until its context ends.
type faultRunner interface{ Run(ctx context.Context) }

// buildRouter wires the provider router of the pod: a capability in PROVIDER_MODE=real comes from the
// vendor adapters (providerwire.Build, after providerwire.ValidateReal), the others from the fakes,
// which read their fault injection from Redis (spec 8.9.6); the returned runner polls it.
func buildRouter(rdb redis.UniversalClient, clk clock.Clock, logger *slog.Logger, pm *provider.Metrics) (*provider.Router, provider.RouterConfig, faultRunner, error) {
	rc, fc, err := providerwire.ParseConfigs()
	if err != nil {
		return nil, rc, nil, err
	}
	faults := redisstore.NewFakeFaults(rdb, fc.FaultsRedisKey, fc.FaultsPollInterval, clk, logger)
	a, err := providerwire.Assemble(rc, fc, realtimeCaps, providerwire.Deps{
		Role: provider.RoleRealtime, Clock: clk, Log: logger, Metrics: pm, Faults: faults,
	})
	if err != nil {
		return nil, rc, nil, err
	}
	router, err := provider.NewRouter(a.Config, a.Deps)
	if err != nil {
		return nil, rc, nil, err
	}
	return router, a.Config, faults, nil
}

// validateProviders is the first check of the process (before any connection): the provider
// configuration of PROVIDER_MODE=real must be able to start (exactly one STT and LLM, keys, models).
func validateProviders() error {
	rc, _, err := providerwire.ParseConfigs()
	if err != nil {
		return fmt.Errorf("provider config: %w", err)
	}
	if err := providerwire.ValidateReal(rc, realtimeCaps); err != nil {
		return fmt.Errorf("provider config: %w", err)
	}
	return nil
}

// voiceCheckConfig is the input of content.ValidateVoices: the enabled TTS vendors are the ones
// providerwire.Build builds (the *_TTS_ENABLED switches).
func voiceCheckConfig(rc provider.RouterConfig, logger *slog.Logger) content.VoiceCheckConfig {
	return content.VoiceCheckConfig{
		RealTTS: rc.ModeFor(provider.CapTTS) == provider.ModeReal,
		Vendors: content.ConfiguredTTSVendors(map[string]bool{
			string(provider.ElevenLabs): rc.ElevenLabsTTSEnabled, string(provider.Cartesia): rc.CartesiaTTSEnabled,
		}),
		Log: logger,
	}
}

// memoryLearner are the M8 ports of the runtime: the memory retriever and the learner service.
type memoryLearner struct {
	memory  *memory.Retriever
	learner *learner.Service
}

// buildMemoryLearner wires the M8 ports of the runtime. The retriever reads memories through the
// plain Postgres reader, keeps the per-session context in Redis (memctx, invalidated by memver) and
// embeds the query through the router (Embedder). The learner service renders the learner block of
// the prompt (PromptContext, Rerender); its content loader is only used by the API's adaptive path.
func buildMemoryLearner(store *postgres.Store, pool *pgxpool.Pool, rdb redis.UniversalClient, embedder memory.Embedder,
	contentRepo content.Repository, clk clock.Clock, ids idgen.Generator, logger *slog.Logger, mm *memory.Metrics) memoryLearner {
	return memoryLearner{
		memory: memory.NewRetriever(memory.RetrieverDeps{
			Reader: postgres.NewMemoryReader(pool), Settings: postgres.NewMemoryProfileSettings(pool, ids, clk),
			Embedder: embedder, Cache: redisstore.NewMemCtxStore(rdb), Clock: clk, Log: logger, Metrics: mm,
		}),
		learner: learner.NewService(learner.Deps{
			Store: store.LearnerStore(), Content: content.NewSessionConfigLoader(contentRepo), Clock: clk, IDs: ids, Log: logger,
		}),
	}
}
