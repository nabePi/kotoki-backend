package provider

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/platform/clock"
)

func snap(h *HealthTracker) []ProviderHealth {
	return h.Snapshot(func(Name, Capability) int { return 125 }, func(Name, Capability) BreakerState { return BreakerClosed })
}

func TestHealthWindow60s(t *testing.T) {
	clk := clock.NewFake(t0)
	h := NewHealthTracker(clk)
	h.Record(OpenAI, CapSTT, nil, 100*time.Millisecond)
	h.Record(OpenAI, CapSTT, &Error{Kind: KindUnavailable}, 200*time.Millisecond)
	s := snap(h)
	require.Len(t, s, 1)
	require.Equal(t, 2, s[0].Window.Requests)
	require.Equal(t, 1, s[0].Window.Successes)
	require.Equal(t, 1, s[0].Window.Failures[KindUnavailable])
	require.NotNil(t, s[0].LastError)
	require.Equal(t, KindUnavailable, s[0].LastError.Kind)
	require.True(t, s[0].LastError.At.Equal(t0))

	clk.Advance(59 * time.Second)
	require.Equal(t, 2, snap(h)[0].Window.Requests, "still inside the window")
	clk.Advance(2 * time.Second) // 61 s after t0
	require.Equal(t, 0, snap(h)[0].Window.Requests, "slid out of the window")
	require.NotNil(t, snap(h)[0].LastError, "last_error outlives the window")
}

func TestHealthBucketsReuse(t *testing.T) {
	clk := clock.NewFake(t0)
	h := NewHealthTracker(clk)
	for i := 0; i < 130; i++ { // wraps the 60-bucket ring twice
		h.Record(OpenAI, CapLLM, nil, time.Millisecond)
		clk.Advance(time.Second)
	}
	require.Equal(t, 59, snap(h)[0].Window.Requests, "only the last 59-60 seconds count; stale buckets were reset")
}

func TestHealthCancelledIsNotAFailure(t *testing.T) {
	h := NewHealthTracker(clock.NewFake(t0))
	h.Record(OpenAI, CapLLM, &Error{Kind: KindCancelled}, time.Millisecond)
	s := snap(h)[0]
	require.Zero(t, s.Window.Successes)
	require.Empty(t, s.Window.Failures)
	require.Nil(t, s.LastError)
}

func TestHealthActiveAndFallback(t *testing.T) {
	h := NewHealthTracker(clock.NewFake(t0))
	h.IncActive(Cartesia, CapTTS)
	h.IncActive(Cartesia, CapTTS)
	h.DecActive(Cartesia, CapTTS)
	h.Fallback(Cartesia, CapTTS)
	h.StreamDisconnect(Cartesia, CapTTS)
	s := snap(h)[0]
	require.Equal(t, 1, s.Active)
	require.Equal(t, 1, s.Window.FallbacksFrom)
	require.Equal(t, 1, s.Window.StreamDisconnects)
}

func TestHealthP95(t *testing.T) {
	h := NewHealthTracker(clock.NewFake(t0))
	for i := 1; i <= 100; i++ {
		h.Record(OpenAI, CapLLM, nil, time.Duration(i)*time.Millisecond)
	}
	p := snap(h)[0].Window.P95LatencyMs
	require.NotNil(t, p)
	require.InDelta(t, 95, *p, 1)
}

func TestHealthSnapshotJSONShape(t *testing.T) {
	clk := clock.NewFake(time.Date(2026, 10, 12, 3, 0, 1, 0, time.UTC))
	h := NewHealthTracker(clk)
	h.Record(OpenAI, CapSTT, &Error{Kind: KindUnavailable}, 0)
	b, err := json.Marshal(snap(h)[0])
	require.NoError(t, err)
	require.JSONEq(t, `{"provider":"OPENAI","capability":"STT","circuit":"CLOSED","active":0,"concurrency_limit":125,
		"window_60s":{"requests":1,"successes":0,"failures":{"UNAVAILABLE":1},"p95_latency_ms":null},
		"last_error":{"kind":"UNAVAILABLE","at":"2026-10-12T03:00:01Z"}}`, string(b))
}

func TestHealthSnapshotSorted(t *testing.T) {
	h := NewHealthTracker(clock.NewFake(t0))
	h.Record(OpenAI, CapSTT, nil, 0)
	h.Record(Cartesia, CapTTS, nil, 0)
	h.Record(ElevenLabs, CapTTS, nil, 0)
	s := snap(h)
	require.Equal(t, []Name{Cartesia, ElevenLabs, OpenAI}, []Name{s[0].Provider, s[1].Provider, s[2].Provider})
}
