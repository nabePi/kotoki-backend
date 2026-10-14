// Package httpapi is the REST layer: typed handlers, middleware, router and DTOs.
// Handlers only decode, call a service and encode; all error output goes through WriteError.
package httpapi

import (
	"context"
	"log/slog"
	"net/netip"

	"github.com/google/uuid"
)

type ctxKey int

const (
	keyRequestID ctxKey = iota
	keyLogger
	keyClientIP
)

// WithRequestID stores the request id used in logs and error bodies.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, keyRequestID, id)
}

// RequestIDFrom returns the request id set by the RequestID middleware, or "".
func RequestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(keyRequestID).(string)
	return id
}

// WithLogger attaches a request-scoped logger.
func WithLogger(ctx context.Context, l *slog.Logger) context.Context {
	return context.WithValue(ctx, keyLogger, l)
}

// LoggerFrom returns the request-scoped logger or slog.Default().
func LoggerFrom(ctx context.Context) *slog.Logger {
	if l, ok := ctx.Value(keyLogger).(*slog.Logger); ok && l != nil {
		return l
	}
	return slog.Default()
}

// WithClientIP stores the resolved client address.
func WithClientIP(ctx context.Context, ip netip.Addr) context.Context {
	return context.WithValue(ctx, keyClientIP, ip)
}

// ClientIPFrom returns the address stored by the ClientIP middleware, if any.
func ClientIPFrom(ctx context.Context) (netip.Addr, bool) {
	ip, ok := ctx.Value(keyClientIP).(netip.Addr)
	return ip, ok
}

func newRequestID() string { return uuid.Must(uuid.NewV7()).String() }
