// Package cartesia is the Cartesia vendor adapter: realtime STT (Ink, spec 8.4.6) and TTS over a
// persistent WebSocket (Sonic, spec 8.4.14). It uses net/http and coder/websocket directly (no
// vendor SDK, D-003) and imports only provider, provider/httpx, platform/otel and the standard
// library.
package cartesia

import (
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/nabePi/kotoki-backend/internal/provider"
)

const (
	defaultBaseURL   = "https://api.cartesia.ai"
	defaultWSBaseURL = "wss://api.cartesia.ai"
	defaultVersion   = "2026-08-14" // the only value the docs list (read 2026-10-16)
	defaultPoolSize  = 80           // PROVIDER_CONCURRENCY_CARTESIA_TTS default
)

// RateLimitSink publishes vendor rate-limit headroom; *provider.Metrics implements it
// (gauge kotoki_provider_ratelimit_remaining{provider,capability,kind}).
type RateLimitSink interface {
	RateLimitRemaining(p provider.Name, c provider.Capability, kind string, v float64)
}

// Config is the Cartesia adapter configuration. TTS model and voice never live here: they come
// from each request (characters.model_name / voice_id, D-015); STTModel comes from
// CARTESIA_STT_MODEL and has no default in code (model names live in configuration only).
type Config struct {
	APIKey    string
	BaseURL   string // zero: https://api.cartesia.ai
	WSBaseURL string // zero: wss://api.cartesia.ai
	Version   string // CARTESIA_VERSION; zero: 2026-08-14

	// STTModel is the realtime transcription model (CARTESIA_STT_MODEL); required by NewSTT.
	STTModel string

	// PoolSize bounds the persistent TTS WebSocket connections (PROVIDER_CONCURRENCY_CARTESIA_TTS,
	// one active context per connection); zero: 80.
	PoolSize int

	// Tap, when set, sees every WebSocket frame (dir "send"/"recv"); used by the recorder.
	Tap func(dir string, text bool, data []byte)

	// SyntheticFinalDelay is the STT safety net when flush_done never answers a finalize
	// (zero: 400 ms). KeepAliveAfter sends 100 ms of silence after that much STT idle time;
	// zero disables it (the docs state no idle limit and silence is billed).
	SyntheticFinalDelay time.Duration
	KeepAliveAfter      time.Duration

	// CancelGrace is how long a cancelled TTS context may take to end (done or error) before its
	// connection is closed instead of reused (zero: 500 ms).
	CancelGrace time.Duration

	// Metrics receives rate-limit headroom read from response headers; nil disables. Cartesia
	// documents no rate-limit headers, so nothing is published today.
	Metrics RateLimitSink
}

// withDefaults fills the zero values.
func (c Config) withDefaults() Config {
	if c.BaseURL == "" {
		c.BaseURL = defaultBaseURL
	}
	if c.WSBaseURL == "" {
		c.WSBaseURL = defaultWSBaseURL
	}
	if c.Version == "" {
		c.Version = defaultVersion
	}
	if c.PoolSize <= 0 {
		c.PoolSize = defaultPoolSize
	}
	c.BaseURL = strings.TrimRight(c.BaseURL, "/")
	c.WSBaseURL = strings.TrimRight(c.WSBaseURL, "/")
	return c
}

// Validate reports a configuration NewSTT/NewTTS would refuse (after defaults).
func (c Config) Validate() error {
	c = c.withDefaults()
	if c.APIKey == "" {
		return errors.New("cartesia: APIKey is empty")
	}
	return nil
}

func (c Config) validateSTT() error {
	if err := c.Validate(); err != nil {
		return err
	}
	if c.STTModel == "" {
		return errors.New("cartesia: STTModel is empty")
	}
	return nil
}

// String never prints the API key.
func (c Config) String() string {
	key := "unset"
	if c.APIKey != "" {
		key = "[redacted]"
	}
	return "cartesia.Config{BaseURL:" + c.BaseURL + " APIKey:" + key + "}"
}

// GoString keeps %#v from printing the API key.
func (c Config) GoString() string { return c.String() }

// LogValue keeps slog from printing the API key.
func (c Config) LogValue() slog.Value { return slog.StringValue(c.String()) }
