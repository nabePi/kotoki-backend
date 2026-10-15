//go:build integration

package redisstore_test

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/conversation/rtstate"
	"github.com/nabePi/kotoki-backend/internal/learner"
	"github.com/nabePi/kotoki-backend/internal/redisstore"
	"github.com/nabePi/kotoki-backend/internal/testutil"
	"github.com/nabePi/kotoki-backend/internal/testutil/repotest"
)

func newRealtimeState(t *testing.T) (*redisstore.RealtimeState, *redis.Client) {
	t.Helper()
	rdb := testutil.NewRedis(t)
	return redisstore.NewRealtimeState(rdb, nil), rdb
}

func TestRealtimeStateRuntimeStoreSuite(t *testing.T) {
	repotest.RuntimeStoreSuite(t, func(t *testing.T) rtstate.Store {
		s, _ := newRealtimeState(t)
		return s
	})
}

func TestOwnerLua(t *testing.T) {
	s, rdb := newRealtimeState(t)
	ctx, sid := context.Background(), uuid.New()
	key := "kotoki:session:" + sid.String() + ":owner"

	ok, err := s.AcquireOwner(ctx, sid, "pod-a", 3, 10*time.Second)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "pod-a|3", rdb.Get(ctx, key).Val())
	pttl := rdb.PTTL(ctx, key).Val()
	require.InDelta(t, float64(10*time.Second), float64(pttl), float64(500*time.Millisecond), "PX is the ttl")

	ok, err = s.AcquireOwner(ctx, sid, "pod-b", 2, 10*time.Second)
	require.NoError(t, err)
	require.False(t, ok, "fence 2 < 3 is refused")
	require.Equal(t, "pod-a|3", rdb.Get(ctx, key).Val(), "refused acquire leaves the owner alone")

	// NOSCRIPT falls back to EVAL.
	require.NoError(t, rdb.ScriptFlush(ctx).Err())
	ok, err = s.AcquireOwner(ctx, sid, "pod-b", 4, 10*time.Second)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "pod-b|4", rdb.Get(ctx, key).Val())

	require.NoError(t, rdb.ScriptFlush(ctx).Err())
	require.NoError(t, s.ReleaseOwner(ctx, sid, "pod-a", 4)) // not the owner
	require.Equal(t, "pod-b|4", rdb.Get(ctx, key).Val())
	require.NoError(t, s.ReleaseOwner(ctx, sid, "pod-b", 3)) // right pod, other fence
	require.Equal(t, "pod-b|4", rdb.Get(ctx, key).Val())
	require.NoError(t, s.ReleaseOwner(ctx, sid, "pod-b", 4))
	require.Zero(t, rdb.Exists(ctx, key).Val())
}

func writeEverything(t *testing.T, s *redisstore.RealtimeState, sid uuid.UUID) {
	t.Helper()
	ctx, ttl := context.Background(), time.Minute
	require.NoError(t, s.WriteRT(ctx, sid, rtstate.RT{Status: "ACTIVE", PodID: "pod-a", Fence: 1}, ttl))
	require.NoError(t, s.WriteTurn(ctx, sid, rtstate.TurnSnap{TurnID: uuid.New(), TurnSeq: 1}, ttl))
	require.NoError(t, s.WriteSummary(ctx, sid, rtstate.Summary{Text: "s"}, ttl))
	require.NoError(t, s.WriteHeartbeat(ctx, sid, time.Now(), ttl))
	_, err := s.AcquireOwner(ctx, sid, "pod-a", 1, 15*time.Second)
	require.NoError(t, err)
	require.NoError(t, s.AppendEvent(ctx, sid, rtstate.EventEntry{Seq: 1, Type: "x", Env: []byte(`{}`)}))
	require.NoError(t, s.WritePodInfo(ctx, "pod-a", rtstate.PodInfo{ActiveSessions: 1, StartedAt: time.Now(), Build: "b"}, 15*time.Second))
}

func TestEveryKeyHasTTL(t *testing.T) {
	s, rdb := newRealtimeState(t)
	ctx, sid := context.Background(), uuid.New()
	writeEverything(t, s, sid)

	keys, err := rdb.Keys(ctx, "kotoki:*").Result()
	require.NoError(t, err)
	require.Len(t, keys, 7, "owner hb rt turn events summary podinfo: %v", keys)
	for _, k := range keys {
		ttl := rdb.PTTL(ctx, k).Val()
		require.Greater(t, ttl, time.Duration(0), "key %s must expire", k)
	}
	require.LessOrEqual(t, rdb.PTTL(ctx, "kotoki:session:"+sid.String()+":events").Val(), 120*time.Second, "stream EXPIRE 120")

	// A rewrite renews the TTL (EXPIRE on every write).
	require.NoError(t, s.WriteRT(ctx, sid, rtstate.RT{Status: "ACTIVE"}, time.Hour))
	require.Greater(t, rdb.PTTL(ctx, "kotoki:session:"+sid.String()+":rt").Val(), 59*time.Minute)
}

func TestStreamMaxLen(t *testing.T) {
	s, rdb := newRealtimeState(t)
	ctx, sid := context.Background(), uuid.New()
	for i := int64(1); i <= 1000; i++ {
		require.NoError(t, s.AppendEvent(ctx, sid, rtstate.EventEntry{Seq: i, PrevSeq: i - 1, Type: "x", Env: []byte(`{}`)}))
	}
	n := rdb.XLen(ctx, "kotoki:session:"+sid.String()+":events").Val()
	require.GreaterOrEqual(t, n, int64(500))
	require.LessOrEqual(t, n, int64(600))
}

func TestRTHashFields(t *testing.T) {
	s, rdb := newRealtimeState(t)
	ctx, sid := context.Background(), uuid.New()
	rt := rtstate.RT{Status: "ACTIVE", TurnState: "LISTENING", AcceptingTurns: true, ConnID: "c1", Connected: false,
		DisconnectedAtMS: 1_760_000_000_000, Generation: 2, NextTurnSeq: 3, CurrentTurnID: uuid.New().String(),
		LastServerSeq: 9, LastDurableSeq: 8, ExpiresAtMS: 1_760_000_300_000, PodID: "pod-a", Fence: 7,
		Adaptive:         learner.AdaptiveDifficulty{AlgorithmVersion: 1, ScenarioDifficulty: 3, TTSSpeakingRate: 0.95, FocusSkills: []learner.Skill{}},
		TurnsSinceAdjust: 2, UpdatedAtMS: 1_760_000_000_500}
	require.NoError(t, s.WriteRT(ctx, sid, rt, time.Minute))

	got, found, err := s.ReadRT(ctx, sid)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, rt, got)

	h := rdb.HGetAll(ctx, "kotoki:session:"+sid.String()+":rt").Val()
	want := []string{"status", "turn_state", "accepting_turns", "conn_id", "connected", "disconnected_at", "generation",
		"next_turn_seq", "current_turn_id", "last_server_seq", "last_durable_seq", "expires_at", "pod_id", "fence",
		"adaptive", "turns_since_adjust", "updated_at"}
	fields := make([]string, 0, len(h))
	for k := range h {
		fields = append(fields, k)
	}
	slices.Sort(fields)
	slices.Sort(want)
	require.Equal(t, want, fields, "exactly the fields of 7.9.1; no stt_vendor (D-017)")
	require.Equal(t, "1", h["accepting_turns"])
	require.Equal(t, "0", h["connected"])
	var ad learner.AdaptiveDifficulty
	require.NoError(t, json.Unmarshal([]byte(h["adaptive"]), &ad))
	require.Equal(t, 3, ad.ScenarioDifficulty)

	// 0 epoch stands for the empty value.
	rt.DisconnectedAtMS = 0
	require.NoError(t, s.WriteRT(ctx, sid, rt, time.Minute))
	require.Empty(t, rdb.HGet(ctx, "kotoki:session:"+sid.String()+":rt", "disconnected_at").Val())
	got, _, err = s.ReadRT(ctx, sid)
	require.NoError(t, err)
	require.Zero(t, got.DisconnectedAtMS)
}

func TestTurnDeleted(t *testing.T) {
	s, rdb := newRealtimeState(t)
	ctx, sid := context.Background(), uuid.New()
	require.NoError(t, s.WriteTurn(ctx, sid, rtstate.TurnSnap{TurnID: uuid.New(), TurnSeq: 1, UserText: "こんにちは"}, time.Minute))
	require.EqualValues(t, 1, rdb.Exists(ctx, "kotoki:session:"+sid.String()+":turn").Val())
	require.NoError(t, s.DeleteTurn(ctx, sid))
	require.Zero(t, rdb.Exists(ctx, "kotoki:session:"+sid.String()+":turn").Val())
	require.NoError(t, s.DeleteTurn(ctx, sid), "deleting a missing turn is not an error")
	_, found, err := s.ReadTurn(ctx, sid)
	require.NoError(t, err)
	require.False(t, found)
}

func TestPubSubTakeover(t *testing.T) {
	s, rdb := newRealtimeState(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := s.SubscribePod(ctx, "pod-a")
	require.NoError(t, err)

	// Wire format of 7.9.1.
	raw := rdb.Subscribe(ctx, "kotoki:realtime:pod:pod-a")
	defer func() { _ = raw.Close() }()
	_, err = raw.Receive(ctx)
	require.NoError(t, err)
	want := rtstate.Takeover{SessionID: uuid.New(), Fence: 42}
	require.NoError(t, s.PublishTakeover(ctx, "pod-a", want))
	msg, err := raw.ReceiveMessage(ctx)
	require.NoError(t, err)
	require.JSONEq(t, fmt.Sprintf(`{"type":"TAKEOVER","session_id":"%s","fence":42}`, want.SessionID), msg.Payload)
	select {
	case got := <-ch:
		require.Equal(t, want, got)
	case <-time.After(2 * time.Second):
		t.Fatal("takeover not delivered")
	}

	// Another pod's channel is not ours.
	require.NoError(t, s.PublishTakeover(ctx, "pod-b", rtstate.Takeover{SessionID: uuid.New(), Fence: 1}))

	// Kill the subscriber connection: the store resubscribes (backoff <= 1 s).
	require.NoError(t, rdb.Do(ctx, "CLIENT", "KILL", "TYPE", "pubsub").Err())
	after := rtstate.Takeover{SessionID: uuid.New(), Fence: 43}
	deadline := time.After(3 * time.Second)
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case got := <-ch:
			require.Equal(t, after, got, "only pod-a takeovers, and the one published after the kill")
			cancel()
			for closed := false; !closed; { // drain the duplicates the ticker published
				select {
				case _, open := <-ch:
					closed = !open
				case <-time.After(2 * time.Second):
					t.Fatal("channel not closed after ctx cancel")
				}
			}
			return
		case <-tick.C:
			_ = s.PublishTakeover(context.Background(), "pod-a", after)
		case <-deadline:
			t.Fatal("no takeover after the connection was killed")
		}
	}
}

func TestKeysUseCanonicalUUID(t *testing.T) {
	s, rdb := newRealtimeState(t)
	ctx, sid := context.Background(), uuid.New()
	writeEverything(t, s, sid)
	re := regexp.MustCompile(`^kotoki:(session:[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}:(owner|hb|rt|turn|events|summary)|realtime:podinfo:pod-a)$`)
	keys, err := rdb.Keys(ctx, "kotoki:*").Result()
	require.NoError(t, err)
	require.NotEmpty(t, keys)
	for _, k := range keys {
		require.Regexp(t, re, k)
		if k != "kotoki:realtime:podinfo:pod-a" {
			require.Contains(t, k, sid.String())
		}
	}
}

func TestRedisTimeout(t *testing.T) {
	s, rdb := newRealtimeState(t)
	ctx, sid := context.Background(), uuid.New()
	// DEBUG SLEEP is disabled on a non-local connection; CLIENT PAUSE stalls the server the same way.
	require.NoError(t, rdb.Do(ctx, "CLIENT", "PAUSE", "2000", "ALL").Err())
	defer rdb.Do(ctx, "CLIENT", "UNPAUSE")

	start := time.Now()
	_, _, err := s.ReadRT(ctx, sid)
	require.Error(t, err)
	require.Less(t, time.Since(start), 250*time.Millisecond)

	start = time.Now()
	require.Error(t, s.WriteHeartbeat(ctx, sid, time.Now(), time.Minute))
	require.Less(t, time.Since(start), 250*time.Millisecond)
}
