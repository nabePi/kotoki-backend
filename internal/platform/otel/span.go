// Package otel wraps OpenTelemetry tracing for feature packages (spec 13.7.2.1):
// they import this package, never the SDK directly. Feature code must create
// spans only via otel.Start and finish them via otel.End: calling
// span.SetAttributes (or RecordError) directly bypasses attribute redaction and
// error-text sanitising, which keep user/AI text, prompts, tokens and emails
// out of exported spans.
package otel

import (
	"context"
	"fmt"
	"runtime/debug"

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
		// Never export err.Error(): the cause chain may carry emails or prompt
		// text. Record a sanitised exception event (code + stack only).
		typ := code
		if typ == "" {
			typ = fmt.Sprintf("%T", err)
		}
		span.AddEvent("exception", trace.WithAttributes(
			semconv.ExceptionType(typ),
			semconv.ExceptionMessage(code),
			semconv.ExceptionStacktrace(string(debug.Stack())),
		))
		span.SetStatus(codes.Error, code)
	}
}

// SetString sets a string attribute on the span in ctx (no span: no-op). The value is redacted
// like Start's attributes, so feature code never reaches span.SetAttributes itself.
func SetString(ctx context.Context, key, value string) { setAttr(ctx, attribute.String(key, value)) }

// SetBool sets a boolean attribute on the span in ctx.
func SetBool(ctx context.Context, key string, value bool) { setAttr(ctx, attribute.Bool(key, value)) }

// SetInt sets an integer attribute on the span in ctx.
func SetInt(ctx context.Context, key string, value int) { setAttr(ctx, attribute.Int(key, value)) }

func setAttr(ctx context.Context, kv attribute.KeyValue) {
	span := trace.SpanFromContext(ctx)
	if !span.IsRecording() {
		return
	}
	span.SetAttributes(redact([]attribute.KeyValue{kv})...)
}
