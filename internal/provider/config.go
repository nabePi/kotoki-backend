package provider

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// Secret is an API key: it never prints (fmt, slog); Reveal returns the value where
// the adapter consumes it.
type Secret string

const secretRedacted = "[REDACTED]"

// String implements fmt.Stringer.
func (Secret) String() string { return secretRedacted }

// GoString implements fmt.GoStringer so %#v cannot leak it either.
func (Secret) GoString() string { return secretRedacted }

// Reveal returns the underlying value.
func (s Secret) Reveal() string { return string(s) }

// Provider modes.
const (
	ModeReal = "real"
	ModeFake = "fake"
)

// ConcurrencyConfig holds PROVIDER_CONCURRENCY_<VENDOR>_<CAP> per real combination
// (spec 8.5.6). Defaults are per capability: STT 125, LLM 60, TTS 80, Evaluation 8,
// Embedding 16.
type ConcurrencyConfig struct {
	OpenAISTT          int `env:"PROVIDER_CONCURRENCY_OPENAI_STT" envDefault:"125"`
	ElevenLabsSTT      int `env:"PROVIDER_CONCURRENCY_ELEVENLABS_STT" envDefault:"125"`
	CartesiaSTT        int `env:"PROVIDER_CONCURRENCY_CARTESIA_STT" envDefault:"125"`
	OpenAILLM          int `env:"PROVIDER_CONCURRENCY_OPENAI_LLM" envDefault:"60"`
	DeepSeekLLM        int `env:"PROVIDER_CONCURRENCY_DEEPSEEK_LLM" envDefault:"60"`
	ElevenLabsTTS      int `env:"PROVIDER_CONCURRENCY_ELEVENLABS_TTS" envDefault:"80"`
	CartesiaTTS        int `env:"PROVIDER_CONCURRENCY_CARTESIA_TTS" envDefault:"80"`
	OpenAIEvaluation   int `env:"PROVIDER_CONCURRENCY_OPENAI_EVALUATION" envDefault:"8"`
	DeepSeekEvaluation int `env:"PROVIDER_CONCURRENCY_DEEPSEEK_EVALUATION" envDefault:"8"`
	OpenAIEmbedding    int `env:"PROVIDER_CONCURRENCY_OPENAI_EMBEDDING" envDefault:"16"`
}

// Limit returns the per-pod concurrency limit of a (vendor, capability). FAKE_*
// instances use the capability default.
func (c ConcurrencyConfig) Limit(n Name, cp Capability) int {
	switch {
	case n == OpenAI && cp == CapSTT:
		return c.OpenAISTT
	case n == ElevenLabs && cp == CapSTT:
		return c.ElevenLabsSTT
	case n == Cartesia && cp == CapSTT:
		return c.CartesiaSTT
	case n == OpenAI && cp == CapLLM:
		return c.OpenAILLM
	case n == DeepSeek && cp == CapLLM:
		return c.DeepSeekLLM
	case n == ElevenLabs && cp == CapTTS:
		return c.ElevenLabsTTS
	case n == Cartesia && cp == CapTTS:
		return c.CartesiaTTS
	case n == OpenAI && cp == CapEvaluation:
		return c.OpenAIEvaluation
	case n == DeepSeek && cp == CapEvaluation:
		return c.DeepSeekEvaluation
	case n == OpenAI && cp == CapEmbedding:
		return c.OpenAIEmbedding
	}
	switch cp { // FAKE_* and anything unlisted: capability default
	case CapSTT:
		return 125
	case CapLLM:
		return 60
	case CapTTS:
		return 80
	case CapEvaluation:
		return 8
	case CapEmbedding:
		return 16
	}
	return 0
}

// RouterConfig is the provider-layer configuration (spec 8.5.1, D-017). There is no
// primary/fallback vendor selection for STT/LLM and no TTS vendor or model in env:
// TTS vendor/model/voice come from each character (D-015).
type RouterConfig struct {
	Mode           string `env:"PROVIDER_MODE" envDefault:"real"`
	ModeSTT        string `env:"PROVIDER_MODE_STT"`
	ModeLLM        string `env:"PROVIDER_MODE_LLM"`
	ModeTTS        string `env:"PROVIDER_MODE_TTS"`
	ModeEvaluation string `env:"PROVIDER_MODE_EVALUATION"`
	ModeEmbedding  string `env:"PROVIDER_MODE_EMBEDDING"`

	OpenAISTTEnabled       bool `env:"OPENAI_STT_ENABLED" envDefault:"true"`
	ElevenLabsSTTEnabled   bool `env:"ELEVENLABS_STT_ENABLED" envDefault:"false"`
	CartesiaSTTEnabled     bool `env:"CARTESIA_STT_ENABLED" envDefault:"false"`
	OpenAILLMEnabled       bool `env:"OPENAI_LLM_ENABLED" envDefault:"true"`
	DeepSeekLLMEnabled     bool `env:"DEEPSEEK_LLM_ENABLED" envDefault:"false"`
	ElevenLabsTTSEnabled   bool `env:"ELEVENLABS_TTS_ENABLED" envDefault:"true"`
	CartesiaTTSEnabled     bool `env:"CARTESIA_TTS_ENABLED" envDefault:"true"`
	OpenAIEmbeddingEnabled bool `env:"OPENAI_EMBEDDING_ENABLED" envDefault:"true"`

	OpenAISTTModel          string `env:"OPENAI_STT_MODEL" envDefault:"gpt-live-transcribe"`
	ElevenLabsSTTModel      string `env:"ELEVENLABS_STT_MODEL" envDefault:"scribe_v2_realtime"`
	CartesiaSTTModel        string `env:"CARTESIA_STT_MODEL" envDefault:"ink-2"`
	OpenAILLMModel          string `env:"OPENAI_LLM_MODEL" envDefault:"gpt-4.1"`
	DeepSeekLLMModel        string `env:"DEEPSEEK_LLM_MODEL" envDefault:"deepseek-flash"`
	OpenAIEvaluationModel   string `env:"OPENAI_EVALUATION_MODEL" envDefault:"gpt-4.1"`
	DeepSeekEvaluationModel string `env:"DEEPSEEK_EVALUATION_MODEL" envDefault:"deepseek-v4-pro"`
	OpenAIEmbeddingModel    string `env:"OPENAI_EMBEDDING_MODEL" envDefault:"text-embedding-3-small"`
	EmbeddingDimensions     int    `env:"EMBEDDING_DIMENSIONS" envDefault:"1536"`

	LLMMaxOutputTokens         int     `env:"LLM_MAX_OUTPUT_TOKENS" envDefault:"300"`
	LLMTemperature             float64 `env:"LLM_TEMPERATURE" envDefault:"0.8"`
	RollingSummaryLLMModel     string  `env:"ROLLING_SUMMARY_LLM_MODEL"`
	OpenAILLMOmitSampling      bool    `env:"OPENAI_LLM_OMIT_SAMPLING" envDefault:"false"`
	OpenAILLMReasoningEffort   string  `env:"OPENAI_LLM_REASONING_EFFORT"`
	DeepSeekLLMThinking        string  `env:"DEEPSEEK_LLM_THINKING" envDefault:"disabled"`
	DeepSeekEvaluationThinking string  `env:"DEEPSEEK_EVALUATION_THINKING" envDefault:"disabled"`

	// TTSFallbackEnabled is the incident/cost switch for characters' voice.fallback.
	TTSFallbackEnabled bool `env:"PROVIDER_TTS_FALLBACK_ENABLED" envDefault:"true"`

	TimeoutSTTConnect       time.Duration `env:"PROVIDER_TIMEOUT_STT_CONNECT" envDefault:"3s"`
	TimeoutSTTFinal         time.Duration `env:"PROVIDER_TIMEOUT_STT_FINAL" envDefault:"1500ms"`
	TimeoutLLMFirstToken    time.Duration `env:"PROVIDER_TIMEOUT_LLM_FIRST_TOKEN" envDefault:"2s"`
	TimeoutLLMTotal         time.Duration `env:"PROVIDER_TIMEOUT_LLM_TOTAL" envDefault:"15s"`
	TimeoutTTSFirstAudio    time.Duration `env:"PROVIDER_TIMEOUT_TTS_FIRST_AUDIO" envDefault:"1500ms"`
	TimeoutTTSSentenceTotal time.Duration `env:"PROVIDER_TIMEOUT_TTS_SENTENCE_TOTAL" envDefault:"10s"`
	TimeoutEvaluation       time.Duration `env:"PROVIDER_TIMEOUT_EVALUATION" envDefault:"60s"`
	TimeoutEmbedding        time.Duration `env:"PROVIDER_TIMEOUT_EMBEDDING" envDefault:"5s"`
	SessionGrace            time.Duration `env:"PROVIDER_SESSION_GRACE" envDefault:"15s"`

	// RotationReplaySpeed paces replayed audio on an STT connection rotation: 0 = no
	// pacing, 4 = 4x realtime.
	RotationReplaySpeed int `env:"PROVIDER_ROTATION_REPLAY_SPEED" envDefault:"0"`

	CBFailureThreshold int           `env:"PROVIDER_CB_FAILURE_THRESHOLD" envDefault:"5"`
	CBWindow           time.Duration `env:"PROVIDER_CB_WINDOW" envDefault:"30s"`
	CBOpenDuration     time.Duration `env:"PROVIDER_CB_OPEN_DURATION" envDefault:"20s"`
	CBHalfOpenProbes   int           `env:"PROVIDER_CB_HALF_OPEN_PROBES" envDefault:"1"`

	PricesFile string `env:"PROVIDER_PRICES_FILE"`

	OpenAIAPIKey     Secret `env:"OPENAI_API_KEY"`
	DeepSeekAPIKey   Secret `env:"DEEPSEEK_API_KEY"`
	ElevenLabsAPIKey Secret `env:"ELEVENLABS_API_KEY"`
	CartesiaAPIKey   Secret `env:"CARTESIA_API_KEY"`

	OpenAIBaseURL       string `env:"OPENAI_BASE_URL" envDefault:"https://api.openai.com/v1"`
	OpenAIWSBaseURL     string `env:"OPENAI_WS_BASE_URL" envDefault:"wss://api.openai.com/v1/realtime"`
	DeepSeekBaseURL     string `env:"DEEPSEEK_BASE_URL" envDefault:"https://api.deepseek.com"`
	ElevenLabsBaseURL   string `env:"ELEVENLABS_BASE_URL" envDefault:"https://api.elevenlabs.io"`
	ElevenLabsWSBaseURL string `env:"ELEVENLABS_WS_BASE_URL" envDefault:"wss://api.elevenlabs.io"`
	CartesiaBaseURL     string `env:"CARTESIA_BASE_URL" envDefault:"https://api.cartesia.ai"`
	CartesiaWSBaseURL   string `env:"CARTESIA_WS_BASE_URL" envDefault:"wss://api.cartesia.ai"`
	CartesiaVersion     string `env:"CARTESIA_VERSION" envDefault:"2026-08-14"`

	Concurrency ConcurrencyConfig
}

// ModeFor returns the mode of a capability: PROVIDER_MODE_<CAP> wins over PROVIDER_MODE,
// and the LLM override also covers EVALUATION when PROVIDER_MODE_EVALUATION is empty.
func (c RouterConfig) ModeFor(cp Capability) string {
	override := ""
	switch cp {
	case CapSTT:
		override = c.ModeSTT
	case CapLLM:
		override = c.ModeLLM
	case CapTTS:
		override = c.ModeTTS
	case CapEvaluation:
		override = c.ModeEvaluation
		if override == "" {
			override = c.ModeLLM
		}
	case CapEmbedding:
		override = c.ModeEmbedding
	}
	if override != "" {
		return override
	}
	return c.Mode
}

func (c RouterConfig) real(cp Capability) bool { return c.ModeFor(cp) == ModeReal }

// ActiveSTT returns the one enabled STT vendor.
func (c RouterConfig) ActiveSTT() (Name, error) {
	var on []Name
	if c.OpenAISTTEnabled {
		on = append(on, OpenAI)
	}
	if c.ElevenLabsSTTEnabled {
		on = append(on, ElevenLabs)
	}
	if c.CartesiaSTTEnabled {
		on = append(on, Cartesia)
	}
	if len(on) != 1 {
		return "", errors.New("exactly one of OPENAI_STT_ENABLED, ELEVENLABS_STT_ENABLED, CARTESIA_STT_ENABLED must be true")
	}
	return on[0], nil
}

// ActiveLLM returns the one enabled LLM vendor (also the Evaluation vendor).
func (c RouterConfig) ActiveLLM() (Name, error) {
	switch {
	case c.OpenAILLMEnabled && !c.DeepSeekLLMEnabled:
		return OpenAI, nil
	case c.DeepSeekLLMEnabled && !c.OpenAILLMEnabled:
		return DeepSeek, nil
	}
	return "", errors.New("exactly one of OPENAI_LLM_ENABLED, DEEPSEEK_LLM_ENABLED must be true")
}

// TTSVendors lists the enabled TTS vendors (those a character may use).
func (c RouterConfig) TTSVendors() []Name {
	var out []Name
	if c.ElevenLabsTTSEnabled {
		out = append(out, ElevenLabs)
	}
	if c.CartesiaTTSEnabled {
		out = append(out, Cartesia)
	}
	return out
}

// STTModel is the model of the active STT vendor ("" when the selection is invalid).
func (c RouterConfig) STTModel() string {
	n, err := c.ActiveSTT()
	if err != nil {
		return ""
	}
	switch n {
	case OpenAI:
		return c.OpenAISTTModel
	case ElevenLabs:
		return c.ElevenLabsSTTModel
	case Cartesia:
		return c.CartesiaSTTModel
	}
	return ""
}

// LLMModel is the model of the active LLM vendor.
func (c RouterConfig) LLMModel() string {
	n, err := c.ActiveLLM()
	if err != nil {
		return ""
	}
	if n == DeepSeek {
		return c.DeepSeekLLMModel
	}
	return c.OpenAILLMModel
}

// EvaluationModel is the evaluation model of the active LLM vendor.
func (c RouterConfig) EvaluationModel() string {
	n, err := c.ActiveLLM()
	if err != nil {
		return ""
	}
	if n == DeepSeek {
		return c.DeepSeekEvaluationModel
	}
	return c.OpenAIEvaluationModel
}

// Validate checks the configuration at startup; every message names the env variable.
// Vendor flags, keys and models are validated only for capabilities in real mode.
func (c RouterConfig) Validate() error {
	var errs []error
	fail := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	for _, m := range []struct{ env, v string }{
		{"PROVIDER_MODE", c.Mode}, {"PROVIDER_MODE_STT", c.ModeSTT}, {"PROVIDER_MODE_LLM", c.ModeLLM},
		{"PROVIDER_MODE_TTS", c.ModeTTS}, {"PROVIDER_MODE_EVALUATION", c.ModeEvaluation}, {"PROVIDER_MODE_EMBEDDING", c.ModeEmbedding},
	} {
		if m.v != "" && m.v != ModeReal && m.v != ModeFake {
			fail("%s must be real or fake, got %q", m.env, m.v)
		}
	}
	needKey := map[Name]bool{}
	need := func(n Name, env, model string) {
		needKey[n] = true
		if strings.TrimSpace(model) == "" {
			fail("%s must not be empty", env)
		}
	}

	if c.real(CapSTT) {
		if n, err := c.ActiveSTT(); err != nil {
			errs = append(errs, err)
		} else {
			switch n {
			case OpenAI:
				need(n, "OPENAI_STT_MODEL", c.OpenAISTTModel)
			case ElevenLabs:
				need(n, "ELEVENLABS_STT_MODEL", c.ElevenLabsSTTModel)
			case Cartesia:
				need(n, "CARTESIA_STT_MODEL", c.CartesiaSTTModel)
			}
		}
	}
	llmReal, evalReal := c.real(CapLLM), c.real(CapEvaluation)
	if llmReal || evalReal {
		if n, err := c.ActiveLLM(); err != nil {
			errs = append(errs, err)
		} else {
			if llmReal {
				if n == DeepSeek {
					need(n, "DEEPSEEK_LLM_MODEL", c.DeepSeekLLMModel)
				} else {
					need(n, "OPENAI_LLM_MODEL", c.OpenAILLMModel)
				}
			}
			if evalReal {
				if n == DeepSeek {
					need(n, "DEEPSEEK_EVALUATION_MODEL", c.DeepSeekEvaluationModel)
				} else {
					need(n, "OPENAI_EVALUATION_MODEL", c.OpenAIEvaluationModel)
				}
			}
		}
	}
	if c.real(CapTTS) {
		if !c.ElevenLabsTTSEnabled && !c.CartesiaTTSEnabled {
			fail("at least one of ELEVENLABS_TTS_ENABLED, CARTESIA_TTS_ENABLED must be true")
		}
		if c.ElevenLabsTTSEnabled {
			needKey[ElevenLabs] = true
		}
		if c.CartesiaTTSEnabled {
			needKey[Cartesia] = true
		}
	}
	if c.real(CapEmbedding) {
		if !c.OpenAIEmbeddingEnabled {
			fail("OPENAI_EMBEDDING_ENABLED must be true")
		}
		need(OpenAI, "OPENAI_EMBEDDING_MODEL", c.OpenAIEmbeddingModel)
		if c.EmbeddingDimensions != 1536 {
			fail("EMBEDDING_DIMENSIONS must be 1536, got %d", c.EmbeddingDimensions)
		}
	}
	for _, k := range []struct {
		n   Name
		env string
		key Secret
	}{
		{OpenAI, "OPENAI_API_KEY", c.OpenAIAPIKey}, {DeepSeek, "DEEPSEEK_API_KEY", c.DeepSeekAPIKey},
		{ElevenLabs, "ELEVENLABS_API_KEY", c.ElevenLabsAPIKey}, {Cartesia, "CARTESIA_API_KEY", c.CartesiaAPIKey},
	} {
		if needKey[k.n] && strings.TrimSpace(k.key.Reveal()) == "" {
			fail("%s is required while a %s capability is enabled in real mode", k.env, k.n)
		}
	}

	for _, t := range []struct {
		env string
		d   time.Duration
	}{
		{"PROVIDER_TIMEOUT_STT_CONNECT", c.TimeoutSTTConnect}, {"PROVIDER_TIMEOUT_STT_FINAL", c.TimeoutSTTFinal},
		{"PROVIDER_TIMEOUT_LLM_FIRST_TOKEN", c.TimeoutLLMFirstToken}, {"PROVIDER_TIMEOUT_LLM_TOTAL", c.TimeoutLLMTotal},
		{"PROVIDER_TIMEOUT_TTS_FIRST_AUDIO", c.TimeoutTTSFirstAudio}, {"PROVIDER_TIMEOUT_TTS_SENTENCE_TOTAL", c.TimeoutTTSSentenceTotal},
		{"PROVIDER_TIMEOUT_EVALUATION", c.TimeoutEvaluation}, {"PROVIDER_TIMEOUT_EMBEDDING", c.TimeoutEmbedding},
		{"PROVIDER_SESSION_GRACE", c.SessionGrace}, {"PROVIDER_CB_WINDOW", c.CBWindow}, {"PROVIDER_CB_OPEN_DURATION", c.CBOpenDuration},
	} {
		if t.d <= 0 {
			fail("%s must be > 0, got %s", t.env, t.d)
		}
	}
	if c.CBFailureThreshold <= 0 {
		fail("PROVIDER_CB_FAILURE_THRESHOLD must be > 0, got %d", c.CBFailureThreshold)
	}
	if c.CBHalfOpenProbes <= 0 {
		fail("PROVIDER_CB_HALF_OPEN_PROBES must be > 0, got %d", c.CBHalfOpenProbes)
	}
	if c.RotationReplaySpeed < 0 {
		fail("PROVIDER_ROTATION_REPLAY_SPEED must be >= 0, got %d", c.RotationReplaySpeed)
	}
	if c.LLMMaxOutputTokens <= 0 {
		fail("LLM_MAX_OUTPUT_TOKENS must be > 0, got %d", c.LLMMaxOutputTokens)
	}
	for _, l := range []struct {
		env string
		n   int
	}{
		{"PROVIDER_CONCURRENCY_OPENAI_STT", c.Concurrency.OpenAISTT}, {"PROVIDER_CONCURRENCY_ELEVENLABS_STT", c.Concurrency.ElevenLabsSTT},
		{"PROVIDER_CONCURRENCY_CARTESIA_STT", c.Concurrency.CartesiaSTT}, {"PROVIDER_CONCURRENCY_OPENAI_LLM", c.Concurrency.OpenAILLM},
		{"PROVIDER_CONCURRENCY_DEEPSEEK_LLM", c.Concurrency.DeepSeekLLM}, {"PROVIDER_CONCURRENCY_ELEVENLABS_TTS", c.Concurrency.ElevenLabsTTS},
		{"PROVIDER_CONCURRENCY_CARTESIA_TTS", c.Concurrency.CartesiaTTS}, {"PROVIDER_CONCURRENCY_OPENAI_EVALUATION", c.Concurrency.OpenAIEvaluation},
		{"PROVIDER_CONCURRENCY_DEEPSEEK_EVALUATION", c.Concurrency.DeepSeekEvaluation}, {"PROVIDER_CONCURRENCY_OPENAI_EMBEDDING", c.Concurrency.OpenAIEmbedding},
	} {
		if l.n <= 0 {
			fail("%s must be > 0, got %d", l.env, l.n)
		}
	}
	return errors.Join(errs...)
}
