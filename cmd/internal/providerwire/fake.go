// Package providerwire builds provider.RouterDeps for the binaries that call providers
// (cmd/realtime, and the worker of M7). Wiring stays manual (D-004); this package only holds
// the part both binaries share: the fake adapters of PROVIDER_MODE=fake (spec 8.5.1, 8.9).
package providerwire

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"

	"github.com/caarlos0/env/v11"

	"github.com/nabePi/kotoki-backend/internal/platform/clock"
	"github.com/nabePi/kotoki-backend/internal/provider"
	"github.com/nabePi/kotoki-backend/internal/provider/fake"
	"github.com/nabePi/kotoki-backend/internal/provider/pricing"
)

// ErrRealModeUnavailable is returned for PROVIDER_MODE=real: the vendor adapters arrive in M9.
var ErrRealModeUnavailable = errors.New("providerwire: PROVIDER_MODE=real: adapter vendor tersedia di M9")

// Deps are the collaborators every adapter and the router share.
type Deps struct {
	Role    provider.ProcessRole
	Clock   clock.Clock
	Log     *slog.Logger
	Metrics *provider.Metrics
	// Faults is the runtime fault-injection source of the fakes (spec 8.9.6); nil = none.
	Faults fake.FaultSource
	// Lookup reads FAKE_<INST>_<CAP>_* variables; nil = os.LookupEnv.
	Lookup func(string) (string, bool)
	// Prices overrides the table loaded from cfg.PricesFile (tests); nil = pricing.Load(cfg.PricesFile).
	Prices provider.PriceTable
}

// ParseConfigs reads RouterConfig and fake.Config from the process environment.
func ParseConfigs() (provider.RouterConfig, fake.Config, error) {
	var rc provider.RouterConfig
	if err := env.Parse(&rc); err != nil {
		return provider.RouterConfig{}, fake.Config{}, err
	}
	var fc fake.Config
	if err := env.Parse(&fc); err != nil {
		return provider.RouterConfig{}, fake.Config{}, err
	}
	return rc, fc, nil
}

// BuildFake builds the fake adapters of caps and fills RouterDeps with them: FAKE_PRIMARY for
// every requested capability and FAKE_FALLBACK for TTS only (the voice.fallback slot). A fake
// instance ignores the *_ENABLED flags (spec 8.5.1, 8.9). EVALUATION always follows LLM (same
// vendor, spec 8.5.1) because provider.NewRouter needs both. A capability that is not requested
// stays nil. PROVIDER_MODE=real for a requested capability is an error until M9.
func BuildFake(cfg provider.RouterConfig, fakeCfg fake.Config, caps []provider.Capability, d Deps) (provider.RouterDeps, error) {
	want := func(c provider.Capability) bool { return slices.Contains(caps, c) }
	for _, c := range caps {
		if cfg.ModeFor(c) == provider.ModeReal || (c == provider.CapLLM && cfg.ModeFor(provider.CapEvaluation) == provider.ModeReal) {
			return provider.RouterDeps{}, fmt.Errorf("%w (%s)", ErrRealModeUnavailable, c)
		}
	}
	if d.Clock == nil {
		d.Clock = clock.System{}
	}
	if d.Log == nil {
		d.Log = slog.New(slog.DiscardHandler)
	}
	if d.Faults == nil {
		d.Faults = fake.NoFaults
	}
	if d.Lookup == nil {
		d.Lookup = os.LookupEnv
	}
	prices := d.Prices
	if prices == nil {
		t, err := pricing.Load(cfg.PricesFile)
		if err != nil {
			return provider.RouterDeps{}, err
		}
		prices = t
	}
	fd := fake.Deps{Clock: d.Clock, Faults: d.Faults, Log: d.Log, Seed: fakeCfg.Seed}
	capOf := func(inst provider.Name, c provider.Capability) (fake.CapConfig, error) {
		return fake.LoadCapConfig(d.Lookup, inst, c)
	}

	rd := provider.RouterDeps{Prices: prices, Clock: d.Clock, Log: d.Log, Role: d.Role, Metrics: d.Metrics}
	if want(provider.CapSTT) {
		c, err := capOf(provider.FakePrimary, provider.CapSTT)
		if err != nil {
			return provider.RouterDeps{}, err
		}
		s, err := fake.NewSTT(provider.FakePrimary, c, fakeCfg, fd)
		if err != nil {
			return provider.RouterDeps{}, err
		}
		rd.STT = s
	}
	if want(provider.CapLLM) {
		c, err := capOf(provider.FakePrimary, provider.CapLLM)
		if err != nil {
			return provider.RouterDeps{}, err
		}
		rd.LLM = fake.NewLLM(provider.FakePrimary, c, fakeCfg, fd)
		ec, err := capOf(provider.FakePrimary, provider.CapEvaluation)
		if err != nil {
			return provider.RouterDeps{}, err
		}
		rd.Evaluation = fake.NewEvaluation(provider.FakePrimary, ec, fakeCfg, fd)
	}
	if want(provider.CapTTS) {
		rd.TTS = map[provider.Name]provider.TTSProvider{}
		for _, inst := range []provider.Name{provider.FakePrimary, provider.FakeFallback} {
			c, err := capOf(inst, provider.CapTTS)
			if err != nil {
				return provider.RouterDeps{}, err
			}
			rd.TTS[inst] = fake.NewTTS(inst, c, fakeCfg, fd)
		}
	}
	if want(provider.CapEmbedding) {
		c, err := capOf(provider.FakePrimary, provider.CapEmbedding)
		if err != nil {
			return provider.RouterDeps{}, err
		}
		rd.Embedding = fake.NewEmbedding(provider.FakePrimary, c, fakeCfg, fd)
	}
	return rd, nil
}
