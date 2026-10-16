//go:build integration

package redisstore_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/memory"
	"github.com/nabePi/kotoki-backend/internal/redisstore"
	"github.com/nabePi/kotoki-backend/internal/testutil"
)

func TestMemCtxStore_RoundTripAndTTL(t *testing.T) {
	rdb := testutil.NewRedis(t)
	s := redisstore.NewMemCtxStore(rdb)
	ctx := context.Background()
	sess, user := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	key := "kotoki:session:" + sess.String() + ":memctx"

	got, err := s.GetContext(ctx, sess)
	require.NoError(t, err)
	require.Nil(t, got, "a missing context is nil, nil")

	mc := memory.MemoryContext{
		SessionID: sess, UserID: user, UserMemVer: 3, CoreTokens: 12, EpisodicTokens: 9, Degraded: true,
		BuiltAt: time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC),
		Core:    []memory.ContextItem{{ID: uuid.Must(uuid.NewV7()), Category: memory.CategoryPreference, Content: "ゆっくり話してほしい。", Score: 0.7, Tokens: 12}},
		Episodic: []memory.ContextItem{{ID: uuid.Must(uuid.NewV7()), Category: memory.CategoryEpisodic, Content: "面接練習。",
			ObservedAt: time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC), Score: 0.8, Tokens: 9}},
	}
	require.NoError(t, s.SetContext(ctx, sess, mc, memory.MemCtxTTL))
	ttl, err := rdb.TTL(ctx, key).Result()
	require.NoError(t, err)
	require.Greater(t, ttl, 4490*time.Second)
	require.LessOrEqual(t, ttl, 4500*time.Second)

	got, err = s.GetContext(ctx, sess)
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, mc.Core, got.Core)
	require.Equal(t, mc.UserID, got.UserID)
	require.Equal(t, int64(3), got.UserMemVer)
	require.True(t, got.Degraded)
	require.True(t, mc.Episodic[0].ObservedAt.Equal(got.Episodic[0].ObservedAt))

	// ttl <= 0 rewrites the value and keeps the TTL
	require.NoError(t, rdb.Expire(ctx, key, 1000*time.Second).Err())
	mc.CoreTokens = 99
	require.NoError(t, s.SetContext(ctx, sess, mc, 0))
	ttl, err = rdb.TTL(ctx, key).Result()
	require.NoError(t, err)
	require.LessOrEqual(t, ttl, 1000*time.Second, "the TTL is kept, not reset")
	require.Greater(t, ttl, 900*time.Second)
	got, _ = s.GetContext(ctx, sess)
	require.Equal(t, 99, got.CoreTokens)

	// ... and never resurrects a key that expired
	other := uuid.Must(uuid.NewV7())
	require.NoError(t, s.SetContext(ctx, other, mc, 0))
	n, err := rdb.Exists(ctx, "kotoki:session:"+other.String()+":memctx").Result()
	require.NoError(t, err)
	require.Zero(t, n)

	// an unreadable entry is a miss
	require.NoError(t, rdb.Set(ctx, key, "{not json", 0).Err())
	got, err = s.GetContext(ctx, sess)
	require.NoError(t, err)
	require.Nil(t, got)
}

func TestMemCtxStore_MemVer(t *testing.T) {
	rdb := testutil.NewRedis(t)
	s := redisstore.NewMemCtxStore(rdb)
	ctx := context.Background()
	u := uuid.Must(uuid.NewV7())
	v, err := s.GetMemVer(ctx, u)
	require.NoError(t, err)
	require.Zero(t, v, "missing memver is 0")
	require.NoError(t, redisstore.NewMemVerBumper(rdb).Bump(ctx, []uuid.UUID{u}))
	require.NoError(t, redisstore.NewMemVerBumper(rdb).Bump(ctx, []uuid.UUID{u}))
	v, err = s.GetMemVer(ctx, u)
	require.NoError(t, err)
	require.Equal(t, int64(2), v)

	require.NoError(t, rdb.Set(ctx, "kotoki:user:"+u.String()+":memver", "abc", 0).Err())
	_, err = s.GetMemVer(ctx, u)
	require.Error(t, err)
}
