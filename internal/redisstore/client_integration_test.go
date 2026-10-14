//go:build integration

package redisstore_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/codes"

	"github.com/nabePi/kotoki-backend/internal/platform/config"
	"github.com/nabePi/kotoki-backend/internal/platform/errs"
	"github.com/nabePi/kotoki-backend/internal/platform/metrics"
	"github.com/nabePi/kotoki-backend/internal/redisstore"
	"github.com/nabePi/kotoki-backend/internal/testutil"
)

func newClient(t *testing.T) (redis.UniversalClient, *prometheus.Registry) {
	t.Helper()
	base := testutil.NewRedis(t)
	reg := prometheus.NewRegistry()
	c, err := redisstore.NewClient(config.Redis{URL: config.Secret("redis://" + base.Options().Addr)}, metrics.NewRedis(reg))
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	return c, reg
}

func counter(t *testing.T, reg *prometheus.Registry, name, label, value string) float64 {
	fams, err := reg.Gather()
	require.NoError(t, err)
	for _, f := range fams {
		if f.GetName() != name {
			continue
		}
		for _, m := range f.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == label && l.GetValue() == value {
					return m.GetCounter().GetValue()
				}
			}
		}
	}
	return 0
}

func TestHookSpan(t *testing.T) {
	rec := testutil.NewTraceRecorder(t)
	c, _ := newClient(t)
	id := uuid.NewString()
	require.NoError(t, c.Set(context.Background(), "kotoki:session:"+id+":rt", "secretvalue", 0).Err())

	span := rec.Find("redis.SET")
	attrs := map[string]string{}
	for _, kv := range span.Attributes {
		attrs[string(kv.Key)] = kv.Value.String()
		require.NotContains(t, kv.Value.String(), id)
		require.NotContains(t, kv.Value.String(), "secretvalue")
	}
	require.Equal(t, "redis", attrs["db.system"])
	require.Equal(t, "kotoki:session:*:rt", attrs["kotoki.redis.key_prefix"])
}

func TestGetMissIsNotError(t *testing.T) {
	rec := testutil.NewTraceRecorder(t)
	c, reg := newClient(t)
	err := c.Get(context.Background(), "kotoki:missing").Err()
	require.ErrorIs(t, err, redis.Nil)
	require.NotEqual(t, codes.Error, rec.Find("redis.GET").Status.Code)
	require.Zero(t, counter(t, reg, "kotoki_redis_command_errors_total", "op", "GET"))
}

func TestCommandErrorCounted(t *testing.T) {
	c, reg := newClient(t)
	err := c.Do(context.Background(), "EVAL", "this is not lua", 0).Err()
	require.Error(t, err)
	require.Equal(t, errs.CodeServiceUnavailable, errs.CodeOf(err))
	require.Equal(t, 1.0, counter(t, reg, "kotoki_redis_command_errors_total", "op", "EVAL"))
}

func TestPipelineSpan(t *testing.T) {
	rec := testutil.NewTraceRecorder(t)
	c, _ := newClient(t)
	_, err := c.Pipelined(context.Background(), func(p redis.Pipeliner) error {
		p.Set(context.Background(), "kotoki:p:1", "a", 0)
		p.Get(context.Background(), "kotoki:p:1")
		return nil
	})
	require.NoError(t, err)
	rec.Find("redis.pipeline")
}

func TestPing(t *testing.T) {
	c, _ := newClient(t)
	require.NoError(t, redisstore.Ping(context.Background(), c))
	require.False(t, errors.Is(redisstore.Ping(context.Background(), c), redis.Nil))
}
