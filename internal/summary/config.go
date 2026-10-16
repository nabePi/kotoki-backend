// Package summary owns the post-session summary worker (spec 9.9.6): transcript and output rules,
// prompt data, the service and the queue consumer. It imports events only (plus prompt and provider
// for the service), never amqp or infrastructure.
package summary

// DefaultMaxChars is the default SUMMARY_MAX_CHARS (spec 9.9.6).
const DefaultMaxChars = 300

// Config are the summary tunables; cmd/worker fills it from platform/config (this package does
// not import platform/config).
type Config struct {
	MaxChars int
}

// DefaultConfig returns the spec 9.9.6 defaults.
func DefaultConfig() Config { return Config{MaxChars: DefaultMaxChars} }
