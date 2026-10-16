package providerwire

import (
	"errors"
	"slices"

	"github.com/nabePi/kotoki-backend/internal/provider"
)

// routerCaps are the five capability slots provider.NewRouter needs filled.
var routerCaps = []provider.Capability{provider.CapSTT, provider.CapLLM, provider.CapEvaluation, provider.CapTTS, provider.CapEmbedding}

// normalizeCaps adds EVALUATION whenever LLM is requested (same vendor, spec 8.5.1) and the other way round.
func normalizeCaps(caps []provider.Capability) []provider.Capability {
	out := slices.Clone(caps)
	if slices.Contains(out, provider.CapLLM) != slices.Contains(out, provider.CapEvaluation) {
		if slices.Contains(out, provider.CapLLM) {
			out = append(out, provider.CapEvaluation)
		} else {
			out = append(out, provider.CapLLM)
		}
	}
	return out
}

// effective forces every router capability that the process does not use (not in caps) to mode fake,
// so that neither validation nor Build asks for flags, keys or models it will never need.
func effective(cfg provider.RouterConfig, caps []provider.Capability) provider.RouterConfig {
	caps = normalizeCaps(caps)
	mode := func(c provider.Capability, dst *string) {
		if !slices.Contains(caps, c) {
			*dst = provider.ModeFake
		} else {
			*dst = cfg.ModeFor(c)
		}
	}
	e := cfg
	mode(provider.CapSTT, &e.ModeSTT)
	mode(provider.CapLLM, &e.ModeLLM)
	mode(provider.CapEvaluation, &e.ModeEvaluation)
	mode(provider.CapTTS, &e.ModeTTS)
	mode(provider.CapEmbedding, &e.ModeEmbedding)
	return e
}

// ValidateReal checks, before any connection is opened, that the capabilities of caps whose mode is
// real can start (spec 8.5.1, D-017): exactly one STT and one LLM vendor, at least one TTS vendor,
// OPENAI embedding, a key for every vendor with an enabled capability, non-empty models, CARTESIA_VERSION
// and 1536 embedding dimensions. A capability in mode fake, or one the process does not use, is not
// validated. Messages name the variables, never their values.
func ValidateReal(cfg provider.RouterConfig, caps []provider.Capability) error {
	e := effective(cfg, caps)
	errs := []error{e.Validate()}
	cartesia := (e.ModeFor(provider.CapSTT) == provider.ModeReal && e.CartesiaSTTEnabled) ||
		(e.ModeFor(provider.CapTTS) == provider.ModeReal && e.CartesiaTTSEnabled)
	if cartesia && e.CartesiaVersion == "" {
		errs = append(errs, errors.New("CARTESIA_VERSION must not be empty while a Cartesia capability is enabled in real mode"))
	}
	return errors.Join(errs...)
}
