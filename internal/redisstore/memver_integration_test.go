//go:build integration

package redisstore_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/redisstore"
	"github.com/nabePi/kotoki-backend/internal/testutil"
)

func TestMemVerBumper_IncrementsAndHasTTL(t *testing.T) {
	rdb := testutil.NewRedis(t)
	b := redisstore.NewMemVerBumper(rdb)
	ctx := context.Background()
	u1, u2 := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())

	require.NoError(t, b.Bump(ctx, []uuid.UUID{u1, u2}))
	require.NoError(t, b.Bump(ctx, []uuid.UUID{u1}))

	v1, err := rdb.Get(ctx, "kotoki:user:"+u1.String()+":memver").Int()
	require.NoError(t, err)
	require.Equal(t, 2, v1)
	v2, err := rdb.Get(ctx, "kotoki:user:"+u2.String()+":memver").Int()
	require.NoError(t, err)
	require.Equal(t, 1, v2)

	for _, u := range []uuid.UUID{u1, u2} {
		ttl, err := rdb.TTL(ctx, "kotoki:user:"+u.String()+":memver").Result()
		require.NoError(t, err)
		require.Greater(t, ttl, 29*24*time.Hour, "every memver key gets a ~30 day TTL")
		require.LessOrEqual(t, ttl, 30*24*time.Hour)
	}
}

func TestMemVerBumper_EmptyIsNoop(t *testing.T) {
	rdb := testutil.NewRedis(t)
	require.NoError(t, redisstore.NewMemVerBumper(rdb).Bump(context.Background(), nil))
	keys, err := rdb.Keys(context.Background(), "kotoki:user:*").Result()
	require.NoError(t, err)
	require.Empty(t, keys)
}

func TestMemVerBumper_RefreshesTTLOnEachBump(t *testing.T) {
	rdb := testutil.NewRedis(t)
	b := redisstore.NewMemVerBumper(rdb)
	ctx := context.Background()
	u := uuid.Must(uuid.NewV7())
	key := "kotoki:user:" + u.String() + ":memver"
	require.NoError(t, b.Bump(ctx, []uuid.UUID{u}))
	require.NoError(t, rdb.Expire(ctx, key, time.Hour).Err())
	require.NoError(t, b.Bump(ctx, []uuid.UUID{u}))
	ttl, err := rdb.TTL(ctx, key).Result()
	require.NoError(t, err)
	require.Greater(t, ttl, 29*24*time.Hour)
}
