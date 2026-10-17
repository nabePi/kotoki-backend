//go:build integration

package driver_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/platform/clock"
	"github.com/nabePi/kotoki-backend/internal/provider"
	"github.com/nabePi/kotoki-backend/internal/redisstore"
	"github.com/nabePi/kotoki-backend/internal/testutil"
	"github.com/nabePi/kotoki-backend/test/chaos/driver"
	"github.com/nabePi/kotoki-backend/test/chaos/faultdoc"
	"github.com/nabePi/kotoki-backend/test/chaos/scenario"
)

// TestRedisDriver_FaultReachesTheFakeProviderPoller sets a fault through the driver and watches the
// poller the fake provider really uses pick it up, then the revert (DEL) remove it.
func TestRedisDriver_FaultReachesTheFakeProviderPoller(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	rdb := testutil.NewRedis(t)
	ff := redisstore.NewFakeFaults(rdb, faultdoc.RedisKey, 20*time.Millisecond, clock.System{}, nil)
	go ff.Run(ctx)
	has := func(c provider.Capability) func() bool {
		return func() bool { _, ok := ff.Current().Lookup(c, provider.FakePrimary); return ok }
	}

	d := driver.Redis{KV: driver.GoRedis{Client: rdb}, Until: time.Hour}
	f1, _ := scenario.ByID("F1")
	require.NoError(t, d.Do(ctx, f1.Inject[scenario.Compose][0]))
	require.Eventually(t, has(provider.CapSTT), 5*time.Second, 25*time.Millisecond)
	require.False(t, has(provider.CapLLM)())
	ttl, err := rdb.TTL(ctx, faultdoc.RedisKey).Result()
	require.NoError(t, err)
	require.Positive(t, ttl, "the key expires by itself")

	require.NoError(t, d.Do(ctx, f1.Revert[scenario.Compose][0]))
	require.Eventually(t, func() bool { return !has(provider.CapSTT)() }, 5*time.Second, 25*time.Millisecond)
}

// TestRedisDriver_ResetDeletesOnlyTheSessionStartBudgets uses a real Redis: SCAN + DEL must remove the
// keys of every user for that policy and nothing else (the limiter keys of other policies, the fault
// document, other applications' keys).
func TestRedisDriver_ResetDeletesOnlyTheSessionStartBudgets(t *testing.T) {
	ctx := t.Context()
	rdb := testutil.NewRedis(t)
	keep := []string{"kotoki:ratelimit:auth_login_ip:10.0.0.1", "kotoki:ratelimit:api_user_default:u1", "kotoki:ratelimit:auth_register_ip:10.0.0.1",
		faultdoc.RedisKey, "other:kotoki:ratelimit:session_create_user:x"}
	drop := []string{"kotoki:ratelimit:session_create_user:u1", "kotoki:ratelimit:session_create_user:u2",
		"kotoki:ratelimit:ticket_user:u1", "kotoki:ratelimit:ws_attach_user:u1", "kotoki:ratelimit:ws_connect_ip:10.0.0.1"}
	for i := 0; i < 700; i++ { // more than one SCAN page
		drop = append(drop, "kotoki:ratelimit:session_create_user:bulk"+time.Duration(i).String())
	}
	for _, k := range append(append([]string{}, keep...), drop...) {
		require.NoError(t, rdb.Set(ctx, k, "1", 0).Err())
	}
	d := driver.Redis{KV: driver.GoRedis{Client: rdb}}
	require.NoError(t, d.Do(ctx, scenario.Action{Kind: scenario.KindReset, Argv: []string{scenario.ResetRateLimits}}))
	for _, k := range keep {
		n, err := rdb.Exists(ctx, k).Result()
		require.NoError(t, err)
		require.EqualValues(t, 1, n, "%s must stay", k)
	}
	for _, k := range drop[:5] {
		n, err := rdb.Exists(ctx, k).Result()
		require.NoError(t, err)
		require.EqualValues(t, 0, n, "%s must go", k)
	}
	left, err := rdb.DBSize(ctx).Result()
	require.NoError(t, err)
	require.EqualValues(t, len(keep), left)
}
