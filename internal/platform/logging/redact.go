package logging

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"reflect"
	"strings"

	"go.opentelemetry.io/otel/trace"
)

const redacted = "[REDACTED]"

// sensitiveKeys is the full redaction list (lowercase, matched case-insensitively
// at any nesting level). Shared with internal/platform/otel via IsSensitiveKey.
var sensitiveKeys = map[string]struct{}{
	"password": {}, "new_password": {}, "token": {}, "access_token": {}, "refresh_token": {},
	"ticket": {}, "authorization": {}, "cookie": {}, "set-cookie": {}, "api_key": {},
	"secret": {}, "email": {}, "phone": {}, "user_text": {}, "assistant_text": {},
	"assistant_spoken_text": {}, "transcript": {}, "partial": {}, "text": {},
	"prompt": {}, "audio": {},
}

// transcriptKeys are the only keys LOG_TRANSCRIPTS=true exempts from redaction.
var transcriptKeys = map[string]struct{}{
	"user_text": {}, "assistant_text": {}, "assistant_spoken_text": {},
	"transcript": {}, "partial": {}, "text": {}, "prompt": {},
}

// IsSensitiveKey reports whether a log/span attribute key must be redacted.
func IsSensitiveKey(key string, logTranscripts bool) bool {
	k := strings.ToLower(key)
	if _, ok := sensitiveKeys[k]; !ok {
		return false
	}
	if logTranscripts {
		if _, ok := transcriptKeys[k]; ok {
			return false
		}
	}
	return true
}

// EmailHash returns the first 32 hex chars of sha256(email), for correlating
// log lines without logging the address.
func EmailHash(email string) string {
	sum := sha256.Sum256([]byte(email))
	return hex.EncodeToString(sum[:])[:32]
}

const redactedComplex = "[REDACTED_COMPLEX]"

type redactHandler struct {
	inner          slog.Handler
	logTranscripts bool
	// denyAll is set once the handler is inside a sensitive group (WithGroup),
	// so every attribute below it is redacted regardless of its own key.
	denyAll bool
}

// NewRedactingHandler wraps inner, redacting sensitive keys (recursively through
// groups) and adding trace_id/span_id from the context span. Composite values
// (maps, structs, slices, raw JSON) are denied by default.
func NewRedactingHandler(inner slog.Handler, logTranscripts bool) slog.Handler {
	return &redactHandler{inner: inner, logTranscripts: logTranscripts}
}

func (h *redactHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.inner.Enabled(ctx, l)
}

func (h *redactHandler) Handle(ctx context.Context, r slog.Record) error {
	nr := slog.NewRecord(r.Time, r.Level, r.Message, r.PC)
	r.Attrs(func(a slog.Attr) bool {
		nr.AddAttrs(h.redact(a, h.denyAll))
		return true
	})
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		nr.AddAttrs(slog.String("trace_id", sc.TraceID().String()), slog.String("span_id", sc.SpanID().String()))
	}
	return h.inner.Handle(ctx, nr)
}

func (h *redactHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	out := make([]slog.Attr, len(attrs))
	for i, a := range attrs {
		out[i] = h.redact(a, h.denyAll)
	}
	return &redactHandler{inner: h.inner.WithAttrs(out), logTranscripts: h.logTranscripts, denyAll: h.denyAll}
}

func (h *redactHandler) WithGroup(name string) slog.Handler {
	return &redactHandler{
		inner:          h.inner.WithGroup(name),
		logTranscripts: h.logTranscripts,
		denyAll:        h.denyAll || IsSensitiveKey(name, h.logTranscripts),
	}
}

// redact applies deny-by-default redaction. deny is true when an ancestor is sensitive.
func (h *redactHandler) redact(a slog.Attr, deny bool) slog.Attr {
	a.Value = a.Value.Resolve()
	if deny || IsSensitiveKey(a.Key, h.logTranscripts) {
		return slog.String(a.Key, redacted)
	}
	switch a.Value.Kind() {
	case slog.KindGroup:
		group := a.Value.Group()
		out := make([]slog.Attr, len(group))
		for i, g := range group {
			out[i] = h.redact(g, false)
		}
		return slog.Attr{Key: a.Key, Value: slog.GroupValue(out...)}
	case slog.KindAny:
		switch v := a.Value.Any().(type) {
		case nil, error:
		case fmt.Stringer:
			// Byte/slice/map-backed Stringers (e.g. json.RawMessage) can carry
			// arbitrary payloads, so only opaque scalar/struct Stringers pass.
			switch reflect.Indirect(reflect.ValueOf(v)).Kind() {
			case reflect.Slice, reflect.Map, reflect.Array, reflect.Invalid:
				return slog.String(a.Key, redactedComplex)
			}
			return slog.String(a.Key, v.String())
		default:
			return slog.String(a.Key, redactedComplex)
		}
	}
	return a
}
