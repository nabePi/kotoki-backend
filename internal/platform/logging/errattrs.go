package logging

import (
	"fmt"
	"log/slog"

	"github.com/nabePi/kotoki-backend/internal/platform/errs"
)

// ErrAttrs returns error_code, error and (when recorded) error_location
// ("<file>:<line> <function>") attributes for err; nil for a nil error.
func ErrAttrs(err error) []slog.Attr {
	if err == nil {
		return nil
	}
	attrs := []slog.Attr{
		slog.String("error_code", string(errs.CodeOf(err))),
		slog.String("error", err.Error()),
	}
	if loc, ok := errs.LocationOf(err); ok {
		attrs = append(attrs, slog.String("error_location", fmt.Sprintf("%s:%d %s", loc.File, loc.Line, loc.Function)))
	}
	return attrs
}
