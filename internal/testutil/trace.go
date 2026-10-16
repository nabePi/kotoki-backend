package testutil

import (
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// TraceRecorder captures finished spans in memory.
type TraceRecorder struct {
	t   testing.TB
	exp *tracetest.InMemoryExporter
}

// NewTraceRecorder installs an in-memory global tracer provider and the W3C trace-context
// propagator (what platform/otel.Setup installs in production), both restored on cleanup.
func NewTraceRecorder(t testing.TB) *TraceRecorder {
	t.Helper()
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	prev := otel.GetTracerProvider()
	prevProp := otel.GetTextMapPropagator()
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() {
		otel.SetTracerProvider(prev)
		otel.SetTextMapPropagator(prevProp)
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
