// Package openai is the OpenAI vendor adapter: STT (realtime transcription), LLM (Chat
// Completions streaming), Evaluation (structured output) and Embedding (spec 8.4). It uses
// net/http directly (no vendor SDK, D-003) and imports only provider, provider/httpx and the
// standard library.
package openai

import (
	"log/slog"
	"time"
)

// Config is the OpenAI adapter configuration. Model names are never part of it: they arrive in
// each request (LLM/Evaluation/Embedding) filled by the router from env.
type Config struct {
	APIKey  string
	BaseURL string // e.g. https://api.openai.com/v1 (endpoint regional or an httptest server)

	// OmitSampling drops temperature and stop from LLM requests (reasoning models reject them).
	OmitSampling bool
	// ReasoningEffort is sent as reasoning_effort when non-empty.
	ReasoningEffort string

	// WSBaseURL is the Realtime endpoint, e.g. wss://api.openai.com/v1/realtime (OPENAI_WS_BASE_URL).
	WSBaseURL string
	// STTModel is the transcription model (OPENAI_STT_MODEL); required by NewSTT.
	STTModel string
	// STTSingleLanguage sends transcription.language (string) instead of transcription.languages
	// (array) for models that only know the older single-language form.
	STTSingleLanguage bool
	// SyntheticFinalDelay is how long after a commit the adapter waits for the vendor before it
	// emits an empty FINAL itself; zero means 400 ms.
	SyntheticFinalDelay time.Duration

	// Tap, when set, sees every WebSocket frame (dir "send"/"recv"); used by the recorder.
	Tap func(dir string, text bool, data []byte)

	// Metrics receives the vendor rate-limit headroom read from response headers; nil disables.
	Metrics RateLimitSink
}

// String never prints the API key.
func (c Config) String() string {
	key := "unset"
	if c.APIKey != "" {
		key = "[redacted]"
	}
	return "openai.Config{BaseURL:" + c.BaseURL + " APIKey:" + key + "}"
}

// GoString keeps %#v from printing the API key.
func (c Config) GoString() string { return c.String() }

// LogValue keeps slog from printing the API key.
func (c Config) LogValue() slog.Value { return slog.StringValue(c.String()) }
