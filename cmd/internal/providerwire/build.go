package providerwire

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"

	"github.com/nabePi/kotoki-backend/internal/provider"
	"github.com/nabePi/kotoki-backend/internal/provider/cartesia"
	"github.com/nabePi/kotoki-backend/internal/provider/deepseek"
	"github.com/nabePi/kotoki-backend/internal/provider/elevenlabs"
	"github.com/nabePi/kotoki-backend/internal/provider/httpx"
	"github.com/nabePi/kotoki-backend/internal/provider/openai"
)

// RealDeps are the vendor adapters of PROVIDER_MODE=real, in the shape of provider.RouterDeps
// (spec 8.5.2): exactly one STT/LLM/Evaluation/Embedding adapter and every enabled TTS vendor
// (D-015: the vendor is chosen per character at runtime). A capability that was not built is nil/empty.
type RealDeps struct {
	STT        provider.STTProvider
	LLM        provider.LLMProvider
	Evaluation provider.EvaluationProvider
	TTS        map[provider.Name]provider.TTSProvider
	Embedding  provider.EmbeddingProvider
}

// TTSVendors lists the built TTS vendors, UPPERCASE and sorted: the "enabled TTS vendors" that the
// startup voice validation (content.ValidateVoices) receives.
func (d RealDeps) TTSVendors() []string {
	out := make([]string, 0, len(d.TTS))
	for n := range d.TTS {
		out = append(out, string(n))
	}
	slices.Sort(out)
	return out
}

// ActiveVendor returns the vendor selected by the *_ENABLED switches (D-017): STT needs exactly one
// of three, LLM exactly one of two, EVALUATION follows LLM, EMBEDDING is always OPENAI. TTS has no
// single vendor (every enabled one is built), so asking for it is an error. A violated "exactly one"
// names every variable of the group.
func ActiveVendor(cfg provider.RouterConfig, c provider.Capability) (provider.Name, error) {
	switch c {
	case provider.CapSTT:
		return cfg.ActiveSTT()
	case provider.CapLLM, provider.CapEvaluation:
		return cfg.ActiveLLM()
	case provider.CapEmbedding:
		return provider.OpenAI, nil
	}
	return "", fmt.Errorf("providerwire: capability %s has no single active vendor", c)
}

// BuildOption tunes Build.
type BuildOption func(*buildOpts)

type buildOpts struct{ metrics *provider.Metrics }

// WithMetrics feeds the adapters' rate-limit headroom gauges (Config.Metrics) into m.
func WithMetrics(m *provider.Metrics) BuildOption { return func(o *buildOpts) { o.metrics = m } }

// Build builds the vendor adapters of the capabilities in caps whose mode is real (spec 8.5.1):
// only the active vendor for STT/LLM/Evaluation (LLM implies Evaluation, same vendor), OPENAI
// for Embedding, every enabled vendor for TTS. A vendor that is not enabled is not built (no key,
// no dial). It does not replace ValidateReal: cmd/* validates first.
func Build(cfg provider.RouterConfig, caps []provider.Capability, log *slog.Logger, opts ...BuildOption) (RealDeps, error) {
	var o buildOpts
	for _, f := range opts {
		f(&o)
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	var sink struct {
		openai     openai.RateLimitSink
		deepseek   deepseek.RateLimitSink
		elevenlabs elevenlabs.RateLimitSink
		cartesia   cartesia.RateLimitSink
	}
	if o.metrics != nil { // a nil *Metrics inside an interface would still be non-nil
		sink.openai, sink.deepseek, sink.elevenlabs, sink.cartesia = o.metrics, o.metrics, o.metrics, o.metrics
	}
	clients := map[provider.Name]*http.Client{} // one client per vendor
	hc := func(n provider.Name) *http.Client {
		if c, ok := clients[n]; ok {
			return c
		}
		c := httpx.NewClient()
		clients[n] = c
		return c
	}
	want := func(c provider.Capability) bool {
		return slices.Contains(caps, c) && cfg.ModeFor(c) == provider.ModeReal
	}
	llmWanted := want(provider.CapLLM) || want(provider.CapEvaluation)

	var d RealDeps
	if want(provider.CapSTT) {
		n, err := ActiveVendor(cfg, provider.CapSTT)
		if err != nil {
			return RealDeps{}, err
		}
		switch n {
		case provider.OpenAI:
			d.STT, err = openai.NewSTT(openai.Config{
				APIKey: cfg.OpenAIAPIKey.Reveal(), BaseURL: cfg.OpenAIBaseURL, WSBaseURL: cfg.OpenAIWSBaseURL,
				STTModel: cfg.OpenAISTTModel, Metrics: sink.openai,
			}, hc(n), log)
		case provider.ElevenLabs:
			d.STT, err = elevenlabs.NewSTT(elevenlabs.Config{
				APIKey: cfg.ElevenLabsAPIKey.Reveal(), BaseURL: cfg.ElevenLabsBaseURL, WSBaseURL: cfg.ElevenLabsWSBaseURL,
				STTModel: cfg.ElevenLabsSTTModel, Metrics: sink.elevenlabs,
			}, hc(n), log)
		case provider.Cartesia:
			d.STT, err = cartesia.NewSTT(cartesia.Config{
				APIKey: cfg.CartesiaAPIKey.Reveal(), BaseURL: cfg.CartesiaBaseURL, WSBaseURL: cfg.CartesiaWSBaseURL,
				Version: cfg.CartesiaVersion, STTModel: cfg.CartesiaSTTModel, Metrics: sink.cartesia,
			}, hc(n), log)
		}
		if err != nil {
			return RealDeps{}, fmt.Errorf("providerwire: STT %s: %w", n, err)
		}
	}
	if llmWanted {
		n, err := ActiveVendor(cfg, provider.CapLLM)
		if err != nil {
			return RealDeps{}, err
		}
		switch n {
		case provider.OpenAI:
			oc := openai.Config{
				APIKey: cfg.OpenAIAPIKey.Reveal(), BaseURL: cfg.OpenAIBaseURL, OmitSampling: cfg.OpenAILLMOmitSampling,
				ReasoningEffort: cfg.OpenAILLMReasoningEffort, Metrics: sink.openai,
			}
			if d.LLM, err = openai.NewLLM(oc, hc(n), log); err == nil {
				d.Evaluation, err = openai.NewEvaluation(oc, hc(n), log)
			}
		case provider.DeepSeek:
			dc := deepseek.Config{
				APIKey: cfg.DeepSeekAPIKey.Reveal(), BaseURL: cfg.DeepSeekBaseURL, Thinking: cfg.DeepSeekLLMThinking,
				EvaluationThinking: cfg.DeepSeekEvaluationThinking, Metrics: sink.deepseek,
			}
			if d.LLM, err = deepseek.NewLLM(dc, hc(n), log); err == nil {
				d.Evaluation, err = deepseek.NewEvaluation(dc, hc(n), log)
			}
		}
		if err != nil {
			return RealDeps{}, fmt.Errorf("providerwire: LLM/EVALUATION %s: %w", n, err)
		}
	}
	if want(provider.CapEmbedding) {
		if !cfg.OpenAIEmbeddingEnabled {
			return RealDeps{}, errors.New("providerwire: OPENAI_EMBEDDING_ENABLED must be true (embedding is always OPENAI)")
		}
		e, err := openai.NewEmbedding(openai.Config{
			APIKey: cfg.OpenAIAPIKey.Reveal(), BaseURL: cfg.OpenAIBaseURL, Metrics: sink.openai,
		}, hc(provider.OpenAI), log)
		if err != nil {
			return RealDeps{}, fmt.Errorf("providerwire: EMBEDDING %s: %w", provider.OpenAI, err)
		}
		d.Embedding = e
	}
	if want(provider.CapTTS) {
		if !cfg.ElevenLabsTTSEnabled && !cfg.CartesiaTTSEnabled {
			return RealDeps{}, errors.New("providerwire: at least one of ELEVENLABS_TTS_ENABLED, CARTESIA_TTS_ENABLED must be true")
		}
		d.TTS = map[provider.Name]provider.TTSProvider{}
		if cfg.ElevenLabsTTSEnabled {
			t, err := elevenlabs.NewTTS(elevenlabs.Config{
				APIKey: cfg.ElevenLabsAPIKey.Reveal(), BaseURL: cfg.ElevenLabsBaseURL, WSBaseURL: cfg.ElevenLabsWSBaseURL,
				Metrics: sink.elevenlabs,
			}, hc(provider.ElevenLabs), log)
			if err != nil {
				return RealDeps{}, fmt.Errorf("providerwire: TTS %s: %w", provider.ElevenLabs, err)
			}
			d.TTS[provider.ElevenLabs] = t
		}
		if cfg.CartesiaTTSEnabled {
			t, err := cartesia.NewTTS(cartesia.Config{
				APIKey: cfg.CartesiaAPIKey.Reveal(), BaseURL: cfg.CartesiaBaseURL, WSBaseURL: cfg.CartesiaWSBaseURL,
				Version: cfg.CartesiaVersion, PoolSize: cfg.Concurrency.CartesiaTTS, Metrics: sink.cartesia,
			}, hc(provider.Cartesia), log)
			if err != nil {
				return RealDeps{}, fmt.Errorf("providerwire: TTS %s: %w", provider.Cartesia, err)
			}
			d.TTS[provider.Cartesia] = t
		}
	}
	return d, nil
}
