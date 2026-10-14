package fake

import (
	"context"
	"log/slog"
)

// countingHandler counts WARN records.
type countingHandler struct{ warns *int }

func (countingHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h countingHandler) Handle(_ context.Context, r slog.Record) error {
	if r.Level == slog.LevelWarn {
		*h.warns++
	}
	return nil
}
func (h countingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h countingHandler) WithGroup(string) slog.Handler      { return h }

func newCountingLogger(warns *int) *slog.Logger { return slog.New(countingHandler{warns}) }
