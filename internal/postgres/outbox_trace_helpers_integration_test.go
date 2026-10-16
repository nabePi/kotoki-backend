//go:build integration

package postgres_test

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

func tracerForTest() trace.Tracer { return otel.Tracer("postgres-test") }

// contextWithRemoteTracestate returns a context that carries a valid remote span context with the
// given tracestate, as an inbound request with W3C headers would.
func contextWithRemoteTracestate(t *testing.T, tracestate string) context.Context {
	t.Helper()
	carrier := propagation.MapCarrier{
		"traceparent": "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
		"tracestate":  tracestate,
	}
	return propagation.TraceContext{}.Extract(context.Background(), carrier)
}
