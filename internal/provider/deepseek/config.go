// Package deepseek is the DeepSeek vendor adapter: LLM (Chat Completions compatible streaming,
// spec 8.4.4) and Evaluation (JSON mode without a strict schema, spec 8.4.9). It uses net/http
// directly (no vendor SDK, D-003) and imports only provider, provider/httpx and the standard
// library; the Chat Completions similarity with OpenAI is duplicated here on purpose (it must
// not import the openai package).
package deepseek

import (
	"fmt"
	"log/slog"

	"github.com/nabePi/kotoki-backend/internal/provider"
)

// Thinking modes accepted by Config.Thinking.
const (
	ThinkingDisabled = "disabled"
	ThinkingEnabled  = "enabled"
)

// RateLimitSink publishes vendor rate-limit headroom; *provider.Metrics implements it
// (gauge kotoki_provider_ratelimit_remaining{provider,capability,kind}).
type RateLimitSink interface {
	RateLimitRemaining(p provider.Name, c provider.Capability, kind string, v float64)
}

// Config is the DeepSeek adapter configuration. Model names are never part of it: they arrive
// in each request, filled by the router from env.
type Config struct {
	APIKey  string
	BaseURL string // e.g. https://api.deepseek.com (or an httptest server)

	// Thinking is "disabled" (default when empty) or "enabled" (DEEPSEEK_LLM_THINKING). DeepSeek
	// enables thinking by default, which adds first-token latency and voids temperature, so the
	// adapter sends thinking:{"type":"disabled"} unless asked otherwise.
	Thinking string

	// EvaluationThinking is the thinking mode of Evaluation requests ("disabled" when empty,
	// DEEPSEEK_EVALUATION_THINKING). Thinking voids temperature, which would make evaluation
	// results non-deterministic, hence disabled by default.
	EvaluationThinking string

	// Tap is reserved for the recorder (HTTP traffic is recorded through replay.Recorder as an
	// http.RoundTripper, so the LLM adapter does not call it).
	Tap func(dir string, text bool, data []byte)

	// Metrics receives rate-limit headroom read from response headers; nil disables. DeepSeek
	// does not document rate-limit headers (spec 8.4.4, limits are concurrency based), so the
	// gauge only moves if the vendor sends the x-ratelimit-remaining-* headers.
	Metrics RateLimitSink
}

// thinkingValue validates a thinking setting; empty means disabled.
func thinkingValue(s string) (string, error) {
	switch s {
	case "", ThinkingDisabled:
		return ThinkingDisabled, nil
	case ThinkingEnabled:
		return ThinkingEnabled, nil
	default:
		return "", fmt.Errorf("deepseek: thinking must be %q or %q, got %q", ThinkingDisabled, ThinkingEnabled, s)
	}
}

// String never prints the API key.
func (c Config) String() string {
	key := "unset"
	if c.APIKey != "" {
		key = "[redacted]"
	}
	return "deepseek.Config{BaseURL:" + c.BaseURL + " APIKey:" + key + "}"
}

// GoString keeps %#v from printing the API key.
func (c Config) GoString() string { return c.String() }

// LogValue keeps slog from printing the API key.
func (c Config) LogValue() slog.Value { return slog.StringValue(c.String()) }
