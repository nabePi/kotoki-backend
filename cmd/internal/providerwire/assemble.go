package providerwire

import (
	"slices"

	"github.com/nabePi/kotoki-backend/internal/provider"
	"github.com/nabePi/kotoki-backend/internal/provider/fake"
)

// Assembled is what a binary needs to start its provider router.
type Assembled struct {
	Deps provider.RouterDeps
	// Config is the RouterConfig to give provider.NewRouter: capabilities the process does not use
	// (not in caps) are in mode fake, because only a stand-in sits in their router slot.
	Config provider.RouterConfig
	// RealTTS reports that TTS is in mode real; TTSVendors are then the built TTS vendors, the input
	// of content.ValidateVoices.
	RealTTS    bool
	TTSVendors []string
}

// Assemble fills provider.RouterDeps for the capabilities of caps: a capability in mode real comes
// from Build, every other router slot (a capability in mode fake, or one the process never calls but
// provider.NewRouter requires, such as STT/TTS in the worker) from BuildFake. ValidateReal runs
// first; it is the only place that decides whether a real configuration may start.
func Assemble(cfg provider.RouterConfig, fakeCfg fake.Config, caps []provider.Capability, d Deps) (Assembled, error) {
	caps = normalizeCaps(caps)
	eff := effective(cfg, caps)
	if err := ValidateReal(cfg, caps); err != nil {
		return Assembled{}, err
	}
	var realCaps []provider.Capability
	for _, c := range routerCaps {
		if slices.Contains(caps, c) && eff.ModeFor(c) == provider.ModeReal {
			realCaps = append(realCaps, c)
		}
	}
	allFake := eff
	allFake.Mode, allFake.ModeSTT, allFake.ModeLLM, allFake.ModeTTS, allFake.ModeEvaluation, allFake.ModeEmbedding = provider.ModeFake, "", "", "", "", ""
	rd, err := BuildFake(allFake, fakeCfg, []provider.Capability{provider.CapSTT, provider.CapLLM, provider.CapTTS, provider.CapEmbedding}, d)
	if err != nil {
		return Assembled{}, err
	}
	var built RealDeps
	if len(realCaps) > 0 {
		if built, err = Build(eff, realCaps, d.Log, WithMetrics(d.Metrics)); err != nil {
			return Assembled{}, err
		}
	}
	a := Assembled{Config: eff, RealTTS: slices.Contains(realCaps, provider.CapTTS)}
	if slices.Contains(realCaps, provider.CapSTT) {
		rd.STT = built.STT
	}
	if slices.Contains(realCaps, provider.CapLLM) {
		rd.LLM = built.LLM
	}
	if slices.Contains(realCaps, provider.CapEvaluation) {
		rd.Evaluation = built.Evaluation
	}
	if slices.Contains(realCaps, provider.CapEmbedding) {
		rd.Embedding = built.Embedding
	}
	if a.RealTTS {
		rd.TTS = built.TTS
		a.TTSVendors = built.TTSVendors()
	}
	a.Deps = rd
	return a, nil
}
