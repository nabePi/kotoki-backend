// Package redisstore is the Redis adapter: client factory and tracing hook.
package redisstore

import (
	"context"
	"errors"
	"net"
	"regexp"
	"strings"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"go.opentelemetry.io/otel/attribute"

	"github.com/nabePi/kotoki-backend/internal/platform/errs"
	"github.com/nabePi/kotoki-backend/internal/platform/metrics"
	"github.com/nabePi/kotoki-backend/internal/platform/otel"
)

var (
	hexRE    = regexp.MustCompile(`^[0-9a-fA-F]{16,}$`)
	digitsRE = regexp.MustCompile(`^[0-9]+$`)
)

// KeyPrefix masks the variable segments of a key (UUIDs, long hex, numbers) so
// it can be used as a low-cardinality span attribute.
func KeyPrefix(key string) string {
	if key == "" {
		return ""
	}
	parts := strings.Split(key, ":")
	for i, p := range parts {
		if _, err := uuid.Parse(p); (err == nil && len(p) == 36) || hexRE.MatchString(p) || digitsRE.MatchString(p) {
			parts[i] = "*"
		}
	}
	return strings.Join(parts, ":")
}

type traceHook struct{ m *metrics.Redis }

// NewTraceHook returns a hook producing one span per command or pipeline and
// counting command errors. Keys and values never reach spans, only KeyPrefix.
func NewTraceHook(m *metrics.Redis) redis.Hook { return &traceHook{m: m} }

func (h *traceHook) DialHook(next redis.DialHook) redis.DialHook {
	return func(ctx context.Context, network, addr string) (net.Conn, error) { return next(ctx, network, addr) }
}

func (h *traceHook) fail(cmd redis.Cmder, err error) error {
	if err == nil || errors.Is(err, redis.Nil) {
		return err
	}
	if h.m != nil {
		h.m.CommandError(strings.ToUpper(cmd.Name()))
		if errors.Is(err, redis.ErrPoolTimeout) {
			h.m.PoolTimeout()
		}
	}
	wrapped := errs.Wrap(err, errs.CodeServiceUnavailable, "redis command failed")
	cmd.SetErr(wrapped)
	return wrapped
}

func (h *traceHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		attrs := []attribute.KeyValue{attribute.String("db.system", "redis")}
		if args := cmd.Args(); len(args) > 1 {
			if key, ok := args[1].(string); ok {
				attrs = append(attrs, attribute.String("kotoki.redis.key_prefix", KeyPrefix(key)))
			}
		}
		ctx, span := otel.Start(ctx, "redis."+strings.ToUpper(cmd.Name()), attrs...)
		err := h.fail(cmd, next(ctx, cmd))
		spanErr := err
		if errors.Is(err, redis.Nil) {
			spanErr = nil // a cache miss is not a failure
		}
		otel.End(span, &spanErr)
		return err
	}
}

func (h *traceHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		ctx, span := otel.Start(ctx, "redis.pipeline", attribute.String("db.system", "redis"))
		err := next(ctx, cmds)
		for _, c := range cmds {
			if cerr := h.fail(c, c.Err()); cerr != nil && !errors.Is(cerr, redis.Nil) && err == nil {
				err = cerr
			}
		}
		spanErr := err
		if errors.Is(err, redis.Nil) {
			spanErr = nil // pipelines report Nil for a missed GET; not a failure
		}
		otel.End(span, &spanErr)
		return err
	}
}
