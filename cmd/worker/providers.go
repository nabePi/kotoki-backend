package main

import (
	"fmt"
	"log/slog"

	"github.com/nabePi/kotoki-backend/cmd/internal/providerwire"
	"github.com/nabePi/kotoki-backend/internal/platform/clock"
	"github.com/nabePi/kotoki-backend/internal/provider"
)

// workerCaps are the capabilities the worker calls: LLM (summary, via EVALUATION following LLM) and
// EMBEDDING (M8 memory). provider.NewRouter requires every adapter, so providerwire.Assemble fills STT
// and TTS with fake stand-ins that the worker never calls.
var workerCaps = []provider.Capability{provider.CapLLM, provider.CapEmbedding}

// buildRouter wires the provider router of the worker (spec 8.5, Role worker: async slot waits and
// retries): LLM/EVALUATION and EMBEDDING come from the vendor adapters in PROVIDER_MODE=real, while
// STT and TTS stay fake stand-ins (providerwire.Assemble). Fault injection (spec 8.9.6) is not
// wired: the worker's fakes use none.
func buildRouter(clk clock.Clock, logger *slog.Logger, pm *provider.Metrics) (*provider.Router, error) {
	rc, fc, err := providerwire.ParseConfigs()
	if err != nil {
		return nil, err
	}
	a, err := providerwire.Assemble(rc, fc, workerCaps, providerwire.Deps{
		Role: provider.RoleWorker, Clock: clk, Log: logger, Metrics: pm,
	})
	if err != nil {
		return nil, err
	}
	return provider.NewRouter(a.Config, a.Deps)
}

// validateProviders checks the provider configuration of PROVIDER_MODE=real before any connection
// is opened; it only matters for a worker that runs consumers (the only ones that call providers).
func validateProviders() error {
	rc, _, err := providerwire.ParseConfigs()
	if err != nil {
		return fmt.Errorf("provider config: %w", err)
	}
	if err := providerwire.ValidateReal(rc, workerCaps); err != nil {
		return fmt.Errorf("provider config: %w", err)
	}
	return nil
}
