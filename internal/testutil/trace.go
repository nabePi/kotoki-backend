package testutil

import (
	"testing"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// TraceRecorder captures finished spans in memory.
type TraceRecorder struct {
	t   testing.TB
	exp *tracetest.InMemoryExporter
}

// NewTraceRecorder installs an in-memory global tracer provider, restored on cleanup.
func NewTraceRecorder(t testing.TB) *TraceRecorder {
	t.Helper()
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() {
		otel.SetTracerProvider(prev)
		_ = tp.Shutdown(t.Context())
	})
	return &TraceRecorder{t: t, exp: exp}
}

// Spans returns all finished spans.
func (r *TraceRecorder) Spans() []tracetest.SpanStub { return r.exp.GetSpans() }

// Find returns the first finished span with the given name, failing if absent.
func (r *TraceRecorder) Find(name string) tracetest.SpanStub {
	r.t.Helper()
	for _, s := range r.Spans() {
		if s.Name == name {
			return s
		}
	}
	r.t.Fatalf("testutil: span %q not found", name)
	return tracetest.SpanStub{}
}
