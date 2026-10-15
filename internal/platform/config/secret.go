// Package config loads and validates per-binary environment configuration.
package config

import "log/slog"

const redacted = "[REDACTED]"

// Secret is a string that never prints its value in logs or formatted output.
type Secret string

// String implements fmt.Stringer.
func (Secret) String() string { return redacted }

// LogValue implements slog.LogValuer.
func (Secret) LogValue() slog.Value { return slog.StringValue(redacted) }

// Reveal returns the underlying value. Use only where the secret is consumed.
func (s Secret) Reveal() string { return string(s) }
