// Package otel wraps OpenTelemetry tracing for feature packages (spec 13.7.2.1):
// they import this package, never the SDK directly.
package otel

import (
	"context"

	otelsdk "go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	semconv "go.opentelemetry.io/otel/semconv/v1.37.0"
	"go.opentelemetry.io/otel/trace"

	"github.com/nabePi/kotoki-backend/internal/platform/errs"
	"github.com/nabePi/kotoki-backend/internal/platform/logging"
)

const (
	tracerName = "github.com/nabePi/kotoki-backend"
	redacted   = "[REDACTED]"

	// AttrErrorCode carries the errs.Code of a finished span's error.
	AttrErrorCode = attribute.Key("kotoki.error_code")
)

// Start begins a child span of ctx. Attribute values whose key is sensitive
// (logging.IsSensitiveKey, transcripts never exempted) are replaced by [REDACTED].
func Start(ctx context.Context, name string, attrs ...attribute.KeyValue) (context.Context, trace.Span) {
	tracer := otelsdk.GetTracerProvider().Tracer(tracerName)
	return tracer.Start(ctx, name, trace.WithAttributes(redact(attrs)...))
}

func redact(attrs []attribute.KeyValue) []attribute.KeyValue {
	out := make([]attribute.KeyValue, len(attrs))
	for i, kv := range attrs {
		if logging.IsSensitiveKey(string(kv.Key), false) {
			kv = attribute.String(string(kv.Key), redacted)
		}
		out[i] = kv
	}
	return out
}

// End finishes span. For a non-nil *errp it records the origin location and
// error code; internal errors (and errors without a registered code) also get
// an exception event with stack trace and Error status, business errors do not.
// Safe with a nil span or nil errp.
func End(span trace.Span, errp *error) {
	if span == nil {
		return
	}
	defer span.End()
	if errp == nil || *errp == nil {
		return
	}
	err := *errp
	code := string(errs.CodeOf(err))
	if loc, ok := errs.LocationOf(err); ok {
		span.SetAttributes(
			semconv.CodeFunctionName(loc.Function),
			semconv.CodeFilePath(loc.File),
			semconv.CodeLineNumber(loc.Line),
		)
	}
	if code != "" {
		span.SetAttributes(AttrErrorCode.String(code))
	}
	if errs.KindOf(err) == errs.KindInternal {
		span.RecordError(err, trace.WithStackTrace(true))
		span.SetStatus(codes.Error, code)
	}
}
