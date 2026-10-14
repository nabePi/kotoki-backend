//go:build integration

package redisstore_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/platform/clock"
	"github.com/nabePi/kotoki-backend/internal/provider"
	"github.com/nabePi/kotoki-backend/internal/redisstore"
	"github.com/nabePi/kotoki-backend/internal/testutil"
)

const faultsKey = "kotoki:fake:faults"

func TestFakeFaultsPollsRedisKey(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rdb := testutil.NewRedis(t)
	ff := redisstore.NewFakeFaults(rdb, faultsKey, 20*time.Millisecond, clock.System{}, nil)
	go ff.Run(ctx)

	has := func() bool {
		_, ok := ff.Current().Lookup(provider.CapSTT, provider.FakePrimary)
		return ok
	}
	require.NoError(t, rdb.Set(ctx, faultsKey, `{"STT.FAKE_PRIMARY":{"mode":"timeout","rate":1}}`, 0).Err())
	require.Eventually(t, has, 5*time.Second, 50*time.Millisecond)
	require.NoError(t, rdb.Del(ctx, faultsKey).Err())
	require.Eventually(t, func() bool { return !has() }, 5*time.Second, 50*time.Millisecond)
}

func TestFakeFaultsMissingKeyIsEmptyNotAnError(t *testing.T) {
	rdb := testutil.NewRedis(t)
	ff := redisstore.NewFakeFaults(rdb, faultsKey, time.Second, clock.System{}, nil)
	ff.PollOnce(context.Background())
	require.Equal(t, 0, ff.Current().Len())
}

func TestFakeFaultsKeepsSnapshotThenClearsDuringOutage(t *testing.T) {
	ctx := context.Background()
	rdb := testutil.NewRedis(t)
	clk := clock.NewFake(time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC))
	ff := redisstore.NewFakeFaults(rdb, faultsKey, time.Second, clk, nil)
	require.NoError(t, rdb.Set(ctx, faultsKey, `{"LLM.FAKE_PRIMARY":{"mode":"error","rate":1}}`, 0).Err())
	ff.PollOnce(ctx)
	require.Equal(t, 1, ff.Current().Len())

	require.NoError(t, rdb.Close()) // every later GET fails: a stand-in for a Redis outage
	clk.Advance(9 * time.Second)
	ff.PollOnce(ctx)
	require.Equal(t, 1, ff.Current().Len(), "the snapshot survives a short outage")
	clk.Advance(2 * time.Second)
	ff.PollOnce(ctx)
	require.Equal(t, 0, ff.Current().Len(), "faults must not stick once Redis has been gone for more than 10 s")
}

func TestFakeFaultsIgnoresBadDocument(t *testing.T) {
	ctx := context.Background()
	rdb := testutil.NewRedis(t)
	ff := redisstore.NewFakeFaults(rdb, faultsKey, time.Second, clock.System{}, nil)
	require.NoError(t, rdb.Set(ctx, faultsKey, `{not json`, 0).Err())
	ff.PollOnce(ctx)
	require.Equal(t, 0, ff.Current().Len())
}
