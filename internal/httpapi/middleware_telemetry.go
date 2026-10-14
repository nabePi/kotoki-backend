package httpapi

import (
	"net/http"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"

	kotelo "github.com/nabePi/kotoki-backend/internal/platform/otel"
)

// Telemetry starts a server span per request, continuing an incoming W3C trace
// context, and renames it to "HTTP {method} {route pattern}" once routing is known.
func Telemetry(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := otel.GetTextMapPropagator().Extract(r.Context(), propagation.HeaderCarrier(r.Header))
		ctx, span := kotelo.Start(ctx, "HTTP "+r.Method)
		defer span.End()
		sw := &statusWriter{ResponseWriter: w}
		next.ServeHTTP(sw, r.WithContext(ctx))
		route := routePattern(r)
		span.SetName("HTTP " + r.Method + " " + route)
		span.SetAttributes(attribute.String("http.request.method", r.Method), attribute.String("http.route", route),
			attribute.Int("http.response.status_code", sw.code()))
		if sw.code() >= 500 {
			span.SetStatus(codes.Error, "")
		}
	})
}
