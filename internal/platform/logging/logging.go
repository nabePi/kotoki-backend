// Package logging builds the service slog.Logger: structured, UTC timestamps,
// mandatory service metadata, trace correlation and key-based redaction.
package logging

import (
	"io"
	"log/slog"
	"time"
)

// Options configures New. Format is "json" (default) or "text".
type Options struct {
	Level                            slog.Level
	Format                           string
	Service, Version, GitCommit, Env string
	LogTranscripts                   bool
}

// New returns a logger writing to w with the redacting handler installed.
func New(w io.Writer, o Options) *slog.Logger {
	ho := &slog.HandlerOptions{
		Level: o.Level,
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if len(groups) == 0 && a.Key == slog.TimeKey && a.Value.Kind() == slog.KindTime {
				a.Value = slog.TimeValue(a.Value.Time().UTC().Truncate(time.Nanosecond))
			}
			return a
		},
	}
	var inner slog.Handler
	if o.Format == "text" {
		inner = slog.NewTextHandler(w, ho)
	} else {
		inner = slog.NewJSONHandler(w, ho)
	}
	h := NewRedactingHandler(inner, o.LogTranscripts).WithAttrs([]slog.Attr{
		slog.String("service", o.Service),
		slog.String("version", o.Version),
		slog.String("git_commit", o.GitCommit),
		slog.String("env", o.Env),
	})
	return slog.New(h)
}
