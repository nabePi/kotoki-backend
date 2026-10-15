package otel

import (
	"context"
	"strings"
	"time"

	otelsdk "go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.37.0"
)

const shutdownTimeout = 3 * time.Second

// Config is the tracer setup input (mirrors the OTEL_* env of the config package).
type Config struct {
	Endpoint           string // OTLP gRPC endpoint; empty disables export
	ServiceName        string
	ResourceAttributes string // "k=v,k2=v2" (OTEL_RESOURCE_ATTRIBUTES)
	Version            string
	Env                string
}

// Setup installs the W3C propagator and, when Endpoint is set, a global tracer
// provider exporting over OTLP gRPC with a batch processor and always_on
// sampling (tail sampling happens in the collector). The returned shutdown is
// never nil. Connecting is lazy, so an unreachable endpoint is not a setup error.
func Setup(ctx context.Context, c Config) (func(context.Context) error, error) {
	otelsdk.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{}))
	if c.Endpoint == "" {
		return func(context.Context) error { return nil }, nil
	}

	opts := []otlptracegrpc.Option{otlptracegrpc.WithInsecure()}
	if strings.Contains(c.Endpoint, "://") {
		opts = append(opts, otlptracegrpc.WithEndpointURL(c.Endpoint))
	} else {
		opts = append(opts, otlptracegrpc.WithEndpoint(c.Endpoint))
	}
	exp, err := otlptracegrpc.New(ctx, opts...)
	if err != nil {
		return nil, err
	}

	attrs := parseResourceAttributes(c.ResourceAttributes)
	attrs = append(attrs,
		semconv.ServiceName(c.ServiceName),
		semconv.ServiceVersion(c.Version),
		semconv.DeploymentEnvironmentName(c.Env),
	)
	res, err := resource.Merge(resource.Default(), resource.NewSchemaless(attrs...))
	if err != nil {
		return nil, err
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
		sdktrace.WithBatcher(exp),
		sdktrace.WithResource(res),
	)
	otelsdk.SetTracerProvider(tp)
	return func(ctx context.Context) error {
		// Bound the final flush so an unreachable collector cannot hang exit.
		ctx, cancel := context.WithTimeout(ctx, shutdownTimeout)
		defer cancel()
		return tp.Shutdown(ctx)
	}, nil
}

// parseResourceAttributes parses "k=v,k2=v2", skipping malformed pairs.
func parseResourceAttributes(s string) []attribute.KeyValue {
	var out []attribute.KeyValue
	for _, pair := range strings.Split(s, ",") {
		k, v, ok := strings.Cut(pair, "=")
		k = strings.TrimSpace(k)
		if !ok || k == "" {
			continue
		}
		out = append(out, attribute.String(k, strings.TrimSpace(v)))
	}
	return out
}
