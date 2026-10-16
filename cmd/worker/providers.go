package main

import (
	"log/slog"

	"github.com/nabePi/kotoki-backend/cmd/internal/providerwire"
	"github.com/nabePi/kotoki-backend/internal/platform/clock"
	"github.com/nabePi/kotoki-backend/internal/provider"
)

// workerCaps are the capabilities the worker router is wired with. provider.NewRouter requires every
// adapter, and the async side uses LLM (summary, via EVALUATION following LLM) and EMBEDDING (M8
// memory); STT and TTS are built only to satisfy it and are never called by the worker.
var workerCaps = []provider.Capability{provider.CapSTT, provider.CapLLM, provider.CapTTS, provider.CapEmbedding}

// buildRouter wires the provider router of the worker (spec 8.5, Role worker: async slot waits and
// retries). M7 knows only PROVIDER_MODE=fake; real mode is refused by providerwire (M9 adds the
// vendor adapters). Fault injection (spec 8.9.6) is not wired: the worker's fakes use none.
func buildRouter(clk clock.Clock, logger *slog.Logger, pm *provider.Metrics) (*provider.Router, error) {
	rc, fc, err := providerwire.ParseConfigs()
	if err != nil {
		return nil, err
	}
	rd, err := providerwire.BuildFake(rc, fc, workerCaps, providerwire.Deps{
		Role: provider.RoleWorker, Clock: clk, Log: logger, Metrics: pm,
	})
	if err != nil {
		return nil, err
	}
	return provider.NewRouter(rc, rd)
}
