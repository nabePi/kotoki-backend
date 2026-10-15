//go:build integration

package redisstore_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/auth"
	"github.com/nabePi/kotoki-backend/internal/platform/clock"
	"github.com/nabePi/kotoki-backend/internal/platform/errs"
	"github.com/nabePi/kotoki-backend/internal/redisstore"
	"github.com/nabePi/kotoki-backend/internal/testutil"
)

var now = time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)

func wsTicket() auth.WSTicket {
	return auth.WSTicket{UserID: uuid.Must(uuid.NewV7()), SessionID: uuid.Must(uuid.NewV7()), IssuedAt: now}
}

func TestTicketStoreIssueConsume(t *testing.T) {
	rdb := testutil.NewRedis(t)
	s := redisstore.NewTicketStore(rdb, clock.NewFake(now))
	ctx := context.Background()
	want := wsTicket()

	raw, err := s.Issue(ctx, want, 30*time.Second)
	require.NoError(t, err)
	require.Len(t, raw, 43)

	sum := sha256.Sum256([]byte(raw))
	key := "kotoki:ws_ticket:" + hex.EncodeToString(sum[:])
	val, err := rdb.Get(ctx, key).Result()
	require.NoError(t, err)
	var body map[string]string
	require.NoError(t, json.Unmarshal([]byte(val), &body))
	require.Equal(t, want.UserID.String(), body["user_id"])
	require.Equal(t, want.SessionID.String(), body["session_id"])
	require.NotEmpty(t, body["issued_at"])
	ttl := rdb.TTL(ctx, key).Val()
	require.Greater(t, ttl, time.Duration(0))
	require.LessOrEqual(t, ttl, 30*time.Second)

	keys, err := rdb.Keys(ctx, "*").Result()
	require.NoError(t, err)
	for _, k := range keys {
		require.False(t, strings.Contains(k, raw), "the plaintext ticket must never be a key")
	}

	got, err := s.Consume(ctx, raw)
	require.NoError(t, err)
	require.Equal(t, want.UserID, got.UserID)
	require.Equal(t, want.SessionID, got.SessionID)
	_, err = s.Consume(ctx, raw)
	require.ErrorIs(t, err, auth.ErrTicketInvalid)
	_, err = s.Consume(ctx, "never-issued")
	require.ErrorIs(t, err, auth.ErrTicketInvalid)
}

func TestTicketConsumeConcurrent(t *testing.T) {
	s := redisstore.NewTicketStore(testutil.NewRedis(t), clock.NewFake(now))
	raw, err := s.Issue(context.Background(), wsTicket(), 30*time.Second)
	require.NoError(t, err)

	var wg sync.WaitGroup
	var ok atomic.Int32
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.Consume(context.Background(), raw); err == nil {
				ok.Add(1)
			}
		}()
	}
	wg.Wait()
	require.Equal(t, int32(1), ok.Load())
}

func TestTicketExpires(t *testing.T) {
	s := redisstore.NewTicketStore(testutil.NewRedis(t), clock.NewFake(now))
	raw, err := s.Issue(context.Background(), wsTicket(), time.Second)
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		_, err := s.Consume(context.Background(), raw)
		return err != nil
	}, 3*time.Second, 100*time.Millisecond)
	_, err = s.Consume(context.Background(), raw)
	require.ErrorIs(t, err, auth.ErrTicketInvalid)
}

func TestTicketRedisDown(t *testing.T) {
	rdb := testutil.NewRedis(t)
	s := redisstore.NewTicketStore(rdb, clock.NewFake(now))
	require.NoError(t, rdb.Close())
	_, err := s.Issue(context.Background(), wsTicket(), 30*time.Second)
	require.Equal(t, errs.CodeServiceUnavailable, errs.CodeOf(err))
	_, err = s.Consume(context.Background(), "whatever")
	require.Equal(t, errs.CodeServiceUnavailable, errs.CodeOf(err))
}
