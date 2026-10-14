//go:build integration

package redisstore_test

import (
	"context"
	"encoding/json"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/redisstore"
	"github.com/nabePi/kotoki-backend/internal/session"
	"github.com/nabePi/kotoki-backend/internal/testutil"
)

func TestHeartbeatReaderMGet(t *testing.T) {
	rdb := testutil.NewRedis(t)
	ctx := context.Background()
	alive, silent, junk := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	beat := time.Date(2026, 10, 14, 10, 5, 0, 123_000_000, time.UTC)
	require.NoError(t, rdb.Set(ctx, redisstore.HeartbeatKey(alive), strconv.FormatInt(beat.UnixMilli(), 10), 60*time.Second).Err())
	require.NoError(t, rdb.Set(ctx, redisstore.HeartbeatKey(junk), "not-a-number", 60*time.Second).Err())
	require.Equal(t, "kotoki:session:"+alive.String()+":hb", redisstore.HeartbeatKey(alive))

	r := redisstore.NewHeartbeatReader(rdb)
	got, err := r.Read(ctx, []uuid.UUID{alive, silent, junk})
	require.NoError(t, err)
	require.Len(t, got, 1, "a missing or unparsable key is absent from the map")
	require.True(t, got[alive].Equal(beat))
	require.Equal(t, time.UTC, got[alive].Location())

	probe, err := r.Read(ctx, nil)
	require.NoError(t, err, "an empty read is a PING")
	require.Empty(t, probe)

	require.NoError(t, rdb.Close())
	_, err = r.Read(ctx, []uuid.UUID{alive})
	require.Error(t, err)
	_, err = r.Read(ctx, nil)
	require.Error(t, err, "the probe fails when Redis is gone")
}

func TestSessionControlPublish(t *testing.T) {
	rdb := testutil.NewRedis(t)
	ctx := context.Background()
	id := uuid.Must(uuid.NewV7())
	sub := rdb.Subscribe(ctx, redisstore.ControlChannel(id))
	defer sub.Close()
	_, err := sub.Receive(ctx) // the subscription is confirmed
	require.NoError(t, err)

	reason := session.EndUserCancelled
	msg := session.ControlMessage{Type: "status_changed", SessionID: id.String(), Status: "CANCELLED", EndReason: string(reason), Version: 7}
	require.NoError(t, redisstore.NewSessionControl(rdb).Publish(ctx, id, msg))

	rctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	got, err := sub.ReceiveMessage(rctx)
	require.NoError(t, err)
	require.Equal(t, "kotoki:session:"+id.String()+":control", got.Channel)
	require.JSONEq(t, `{"type":"status_changed","session_id":"`+id.String()+`","status":"CANCELLED","end_reason":"USER_CANCELLED","version":7}`, got.Payload)

	var back session.ControlMessage
	require.NoError(t, json.Unmarshal([]byte(got.Payload), &back))
	require.Equal(t, msg, back)
}
