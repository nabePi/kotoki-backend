package repotest

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/conversation/rtstate"
)

// RuntimeStoreSuite is the contract of rtstate.Store (spec 7.9), run against the in-memory fake
// and the Redis implementation alike.
func RuntimeStoreSuite(t *testing.T, mk func(t *testing.T) rtstate.Store) {
	const ttl = time.Minute
	Each(t, mk,
		Case[rtstate.Store]{"owner fencing", func(t *testing.T, s rtstate.Store) {
			ctx, sid := t.Context(), uuid.New()
			ok, err := s.AcquireOwner(ctx, sid, "pod-a", 3, ttl)
			require.NoError(t, err)
			require.True(t, ok)
			ok, err = s.AcquireOwner(ctx, sid, "pod-b", 2, ttl)
			require.NoError(t, err)
			require.False(t, ok, "older fence must not take over")
			ok, err = s.AcquireOwner(ctx, sid, "pod-a", 3, ttl) // refresh, same fence
			require.NoError(t, err)
			require.True(t, ok)
			ok, err = s.AcquireOwner(ctx, sid, "pod-b", 4, ttl) // takeover
			require.NoError(t, err)
			require.True(t, ok)

			require.NoError(t, s.ReleaseOwner(ctx, sid, "pod-a", 3)) // not the owner: no-op
			ok, _ = s.AcquireOwner(ctx, sid, "pod-c", 3, ttl)
			require.False(t, ok, "pod-b still owns")
			require.NoError(t, s.ReleaseOwner(ctx, sid, "pod-b", 4))
			ok, err = s.AcquireOwner(ctx, sid, "pod-c", 1, ttl)
			require.NoError(t, err)
			require.True(t, ok, "released owner leaves no fence behind")
		}},
		Case[rtstate.Store]{"heartbeat", func(t *testing.T, s rtstate.Store) {
			ctx, sid := t.Context(), uuid.New()
			_, found, err := s.ReadHeartbeat(ctx, sid)
			require.NoError(t, err)
			require.False(t, found)
			at := time.UnixMilli(1_760_000_000_123).UTC()
			require.NoError(t, s.WriteHeartbeat(ctx, sid, at, ttl))
			got, found, err := s.ReadHeartbeat(ctx, sid)
			require.NoError(t, err)
			require.True(t, found)
			require.True(t, at.Equal(got))
		}},
		Case[rtstate.Store]{"rt turn summary roundtrip", func(t *testing.T, s rtstate.Store) {
			ctx, sid := t.Context(), uuid.New()
			_, found, err := s.ReadRT(ctx, sid)
			require.NoError(t, err)
			require.False(t, found)
			rt := rtstate.RT{Status: "ACTIVE", TurnState: "LISTENING", AcceptingTurns: true, ConnID: "c1", Connected: true, Generation: 2,
				NextTurnSeq: 5, CurrentTurnID: uuid.New().String(), LastServerSeq: 9, LastDurableSeq: 8, ExpiresAtMS: 1_760_000_300_000,
				PodID: "pod-a", Fence: 3, TurnsSinceAdjust: 1, UpdatedAtMS: 1_760_000_000_000}
			require.NoError(t, s.WriteRT(ctx, sid, rt, ttl))
			got, found, err := s.ReadRT(ctx, sid)
			require.NoError(t, err)
			require.True(t, found)
			require.Equal(t, rt, got)

			ts := rtstate.TurnSnap{TurnID: uuid.New(), TurnSeq: 4, Initiator: "USER", TurnState: "THINKING", Generation: 2,
				UserStartedAtMS: 1_760_000_000_000, UserText: "こんにちは", AssistantText: "はい", SpokenSentences: 1, UpdatedAtMS: 1_760_000_000_500}
			require.NoError(t, s.WriteTurn(ctx, sid, ts, ttl))
			gt, found, err := s.ReadTurn(ctx, sid)
			require.NoError(t, err)
			require.True(t, found)
			require.Equal(t, ts, gt)
			require.NoError(t, s.DeleteTurn(ctx, sid))
			_, found, _ = s.ReadTurn(ctx, sid)
			require.False(t, found)

			sum := rtstate.Summary{Text: "ringkasan", CoveredUntilSeq: 12, Model: "m", UpdatedAtMS: 1_760_000_001_000}
			require.NoError(t, s.WriteSummary(ctx, sid, sum, ttl))
			gs, found, err := s.ReadSummary(ctx, sid)
			require.NoError(t, err)
			require.True(t, found)
			require.Equal(t, sum, gs)

			_, found, err = s.ReadMemCtx(ctx, sid)
			require.NoError(t, err)
			require.False(t, found)
			require.NoError(t, s.WritePodInfo(ctx, "pod-a", rtstate.PodInfo{ActiveSessions: 2, StartedAt: time.Now().UTC(), Build: "x"}, ttl))
		}},
		Case[rtstate.Store]{"events stream", func(t *testing.T, s rtstate.Store) {
			ctx, sid := t.Context(), uuid.New()
			for i := int64(1); i <= 600; i++ {
				require.NoError(t, s.AppendEvent(ctx, sid, rtstate.EventEntry{Seq: i, PrevSeq: i - 1, Type: "x", Env: []byte(`{"a":1}`)}))
			}
			evs, err := s.ReadEvents(ctx, sid)
			require.NoError(t, err)
			require.GreaterOrEqual(t, len(evs), 500)
			for i := 1; i < len(evs); i++ {
				require.Less(t, evs[i-1].Seq, evs[i].Seq)
			}
			require.EqualValues(t, 600, evs[len(evs)-1].Seq)
			require.Equal(t, []byte(`{"a":1}`), evs[len(evs)-1].Env)
		}},
		Case[rtstate.Store]{"takeover pubsub", func(t *testing.T, s rtstate.Store) {
			ctx := t.Context()
			ch, err := s.SubscribePod(ctx, "pod-a")
			require.NoError(t, err)
			want := rtstate.Takeover{SessionID: uuid.New(), Fence: 42}
			require.NoError(t, s.PublishTakeover(ctx, "pod-a", want))
			select {
			case got := <-ch:
				require.Equal(t, want, got)
			case <-time.After(2 * time.Second):
				t.Fatal("takeover not delivered")
			}
		}},
	)
}
