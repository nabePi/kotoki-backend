// Package elevenlabs is the ElevenLabs vendor adapter: TTS over HTTP streaming per sentence
// (spec 8.4.5) and realtime STT (spec 8.4.1). It uses net/http and coder/websocket directly (no
// vendor SDK, D-003) and imports only provider, provider/httpx, platform/otel and the standard
// library.
package elevenlabs

import (
	"log/slog"
	"time"

	"github.com/nabePi/kotoki-backend/internal/provider"
)

// RateLimitSink publishes vendor rate-limit headroom; *provider.Metrics implements it
// (gauge kotoki_provider_ratelimit_remaining{provider,capability,kind}).
type RateLimitSink interface {
	RateLimitRemaining(p provider.Name, c provider.Capability, kind string, v float64)
}

// Config is the ElevenLabs adapter configuration. Model and voice never live here: TTS reads
// them from each request (characters.model_name / voice_id, D-015).
type Config struct {
	APIKey    string
	BaseURL   string // e.g. https://api.elevenlabs.io (ELEVENLABS_BASE_URL or an httptest server)
	WSBaseURL string // e.g. wss://api.elevenlabs.io (ELEVENLABS_WS_BASE_URL); used by STT

	// STTModel is the realtime transcription model (ELEVENLABS_STT_MODEL); required by NewSTT.
	STTModel string

	// Tap, when set, sees every WebSocket frame (dir "send"/"recv"); used by the recorder (STT).
	Tap func(dir string, text bool, data []byte)

	// SyntheticFinalDelay and KeepAliveAfter are STT timers (zero: 400 ms / 5 s).
	SyntheticFinalDelay time.Duration
	KeepAliveAfter      time.Duration

	// Metrics receives rate-limit headroom read from response headers; nil disables.
	// ElevenLabs does not document rate-limit headers, so the gauge only moves if the vendor
	// sends x-ratelimit-remaining-requests / x-ratelimit-remaining-tokens.
	Metrics RateLimitSink
}

// String never prints the API key.
func (c Config) String() string {
	key := "unset"
	if c.APIKey != "" {
		key = "[redacted]"
	}
	return "elevenlabs.Config{BaseURL:" + c.BaseURL + " APIKey:" + key + "}"
}

// GoString keeps %#v from printing the API key.
func (c Config) GoString() string { return c.String() }

// LogValue keeps slog from printing the API key.
func (c Config) LogValue() slog.Value { return slog.StringValue(c.String()) }
