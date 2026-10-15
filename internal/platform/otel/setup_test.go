package otel

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	otelsdk "go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
)

func TestSetupNoEndpointIsNoop(t *testing.T) {
	shutdown, err := Setup(context.Background(), Config{ServiceName: "t"})
	require.NoError(t, err)
	require.NotNil(t, shutdown)
	assert.NotPanics(t, func() {
		_, sp := Start(context.Background(), "noop")
		sp.End()
	})
	assert.Contains(t, otelsdk.GetTextMapPropagator().Fields(), propagation.TraceContext{}.Fields()[0])
	assert.NoError(t, shutdown(context.Background()))
}

func TestSetupWithEndpoint(t *testing.T) {
	shutdown, err := Setup(context.Background(), Config{
		Endpoint: "localhost:1", ServiceName: "kotoki-test", Version: "v0", Env: "test",
		ResourceAttributes: "k8s.pod.name=p1, bad,team=x",
	})
	require.NoError(t, err)
	_, sp := Start(context.Background(), "s")
	sp.End()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() { _ = shutdown(ctx); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown took >= 5s")
	}
}

func TestParseResourceAttributes(t *testing.T) {
	kvs := parseResourceAttributes("a=1, b = 2,bad,=x,c=")
	got := map[string]string{}
	for _, kv := range kvs {
		got[string(kv.Key)] = kv.Value.AsString()
	}
	assert.Equal(t, map[string]string{"a": "1", "b": "2", "c": ""}, got)
}
