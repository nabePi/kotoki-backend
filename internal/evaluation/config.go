package evaluation

import "time"

// Config are the evaluation tunables; cmd/worker fills it from platform/config (this package does
// not import platform/config).
type Config struct {
	MinUserTurns         int
	MinUserSpeechSeconds int
	MinActiveSeconds     int
	MinFindingConfidence float64
	TranscriptMaxChars   int
	HandlerTimeout       time.Duration
}

// DefaultConfig returns the spec 10.13 defaults.
func DefaultConfig() Config {
	return Config{
		MinUserTurns: 3, MinUserSpeechSeconds: 20, MinActiveSeconds: 60,
		MinFindingConfidence: 0.6, TranscriptMaxChars: 60000, HandlerTimeout: 200 * time.Second,
	}
}

// Thresholds converts the evidence minimums for Stats.Insufficient.
func (c Config) Thresholds() Thresholds {
	return Thresholds{MinUserTurns: c.MinUserTurns, MinSpeechSeconds: c.MinUserSpeechSeconds, MinActiveSeconds: c.MinActiveSeconds}
}
