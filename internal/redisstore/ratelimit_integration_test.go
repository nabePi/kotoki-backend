//go:build integration

package redisstore_test

import (
	"bytes"
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/platform/clock"
	"github.com/nabePi/kotoki-backend/internal/platform/metrics"
	"github.com/nabePi/kotoki-backend/internal/platform/ratelimit"
	"github.com/nabePi/kotoki-backend/internal/redisstore"
	rtestutil "github.com/nabePi/kotoki-backend/internal/testutil"
)

func newLimiter(t *testing.T) (*redisstore.RateLimiter, *prometheus.Registry, *bytes.Buffer, *clock.Fake) {
	t.Helper()
	rdb := rtestutil.NewRedis(t)
	clk := clock.NewFake(now)
	reg := prometheus.NewRegistry()
	logs := &bytes.Buffer{}
	l := redisstore.NewRateLimiter(rdb, ratelimit.NewLocal(clk, 1000), clk, slog.New(slog.NewTextHandler(logs, nil)), metrics.NewRateLimit(reg))
	return l, reg, logs, clk
}

func TestRedisLimiterBurstThenDeny(t *testing.T) {
	l, _, _, _ := newLimiter(t)
	p := ratelimit.Policy{Name: "burst", Capacity: 3, Refill: 3, Period: time.Hour}
	ctx := context.Background()
	for _, remaining := range []int{2, 1, 0} {
		d, err := l.Allow(ctx, p, "s")
		require.NoError(t, err)
		require.True(t, d.Allowed)
		require.Equal(t, remaining, d.Remaining)
	}
	d, err := l.Allow(ctx, p, "s")
	require.NoError(t, err)
	require.False(t, d.Allowed)
	require.Greater(t, d.RetryAfter, time.Duration(0))
}

func TestRedisLimiterRefill(t *testing.T) {
	l, _, _, _ := newLimiter(t)
	p := ratelimit.Policy{Name: "refill", Capacity: 1, Refill: 1, Period: 200 * time.Millisecond}
	ctx := context.Background()
	d, _ := l.Allow(ctx, p, "s")
	require.True(t, d.Allowed)
	d, _ = l.Allow(ctx, p, "s")
	require.False(t, d.Allowed)
	require.Eventually(t, func() bool {
		d, err := l.Allow(ctx, p, "s")
		return err == nil && d.Allowed
	}, 3*time.Second, 50*time.Millisecond)
}

func TestRedisLimiterKeyHasTTL(t *testing.T) {
	rdb := rtestutil.NewRedis(t)
	clk := clock.NewFake(now)
	l := redisstore.NewRateLimiter(rdb, ratelimit.NewLocal(clk, 10), clk, slog.New(slog.DiscardHandler), nil)
	p := ratelimit.Policy{Name: "ttl", Capacity: 3, Refill: 3, Period: time.Hour}
	_, err := l.Allow(context.Background(), p, "subject-1")
	require.NoError(t, err)
	ttl := rdb.PTTL(context.Background(), "kotoki:ratelimit:ttl:subject-1").Val()
	require.Greater(t, ttl, time.Duration(0))
}

func TestRedisLimiterSubjectIsolation(t *testing.T) {
	l, _, _, _ := newLimiter(t)
	p := ratelimit.Policy{Name: "iso", Capacity: 1, Refill: 1, Period: time.Hour}
	ctx := context.Background()
	d, _ := l.Allow(ctx, p, "a")
	require.True(t, d.Allowed)
	d, _ = l.Allow(ctx, p, "a")
	require.False(t, d.Allowed)
	d, _ = l.Allow(ctx, p, "b")
	require.True(t, d.Allowed)
}

func TestRedisLimiterFallbackWhenDown(t *testing.T) {
	rdb := rtestutil.NewRedis(t)
	clk := clock.NewFake(now)
	reg := prometheus.NewRegistry()
	logs := &bytes.Buffer{}
	l := redisstore.NewRateLimiter(rdb, ratelimit.NewLocal(clk, 100), clk, slog.New(slog.NewTextHandler(logs, nil)), metrics.NewRateLimit(reg))
	require.NoError(t, rdb.Close())

	p := ratelimit.Policy{Name: "x", Capacity: 2, Refill: 2, Period: time.Hour}
	allowed, denied := 0, 0
	for i := 0; i < 5; i++ {
		d, err := l.Allow(context.Background(), p, "s")
		require.NoError(t, err, "a dead Redis must never surface as an error (fail-open to the local limiter)")
		if d.Allowed {
			allowed++
		} else {
			denied++
		}
	}
	require.Equal(t, 2, allowed)
	require.Equal(t, 3, denied)
	require.Equal(t, 5.0, counterValue(t, reg, "kotoki_ratelimit_backend_errors_total"))
	require.Equal(t, 3.0, counterValue(t, reg, "kotoki_ratelimit_rejected_total"))
	require.Equal(t, 1, bytes.Count(logs.Bytes(), []byte("level=WARN")), "warn at most once per 10s")

	clk.Advance(11 * time.Second)
	_, err := l.Allow(context.Background(), p, "s")
	require.NoError(t, err)
	require.Equal(t, 2, bytes.Count(logs.Bytes(), []byte("level=WARN")), "after 10s the warning is allowed again")
}

func counterValue(t *testing.T, reg *prometheus.Registry, name string) float64 {
	t.Helper()
	fams, err := reg.Gather()
	require.NoError(t, err)
	for _, f := range fams {
		if f.GetName() == name {
			return f.GetMetric()[0].GetCounter().GetValue()
		}
	}
	t.Fatalf("metric %s not found", name)
	return 0
}
