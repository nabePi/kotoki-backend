package provider

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/platform/clock"
)

var t0 = time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)

func testBreaker(clk clock.Clock, on func(from, to BreakerState)) *Breaker {
	return newBreaker(BreakerConfig{Threshold: 5, Window: 30 * time.Second, OpenDuration: 20 * time.Second, HalfOpenProbes: 1}, clk, on)
}

func failN(t *testing.T, b *Breaker, n int, err error) {
	t.Helper()
	for i := 0; i < n; i++ {
		tk, ok := b.Allow()
		require.True(t, ok)
		tk.Done(err)
	}
}

var unavailable = &Error{Kind: KindUnavailable}

func TestBreakerStateMachine(t *testing.T) {
	t.Run("opens at threshold, probes, closes", func(t *testing.T) {
		clk := clock.NewFake(t0)
		b := testBreaker(clk, nil)
		failN(t, b, 4, unavailable)
		require.Equal(t, BreakerClosed, b.State())
		failN(t, b, 1, unavailable)
		require.Equal(t, BreakerOpen, b.State())
		_, ok := b.Allow()
		require.False(t, ok)
		clk.Advance(20 * time.Second)
		tk, ok := b.Allow()
		require.True(t, ok)
		require.Equal(t, BreakerHalfOpen, b.State())
		_, ok = b.Allow()
		require.False(t, ok, "probe in flight")
		tk.Done(nil)
		require.Equal(t, BreakerClosed, b.State())
	})

	t.Run("window slides", func(t *testing.T) {
		clk := clock.NewFake(t0)
		b := testBreaker(clk, nil)
		failN(t, b, 4, unavailable)
		clk.Advance(31 * time.Second)
		failN(t, b, 1, unavailable)
		require.Equal(t, BreakerClosed, b.State())
	})

	t.Run("uncounted failures are ignored", func(t *testing.T) {
		clk := clock.NewFake(t0)
		b := testBreaker(clk, nil)
		failN(t, b, 10, &Error{Kind: KindCancelled})
		failN(t, b, 10, &Error{Kind: KindContentFiltered})
		failN(t, b, 10, &Error{Kind: KindRateLimited, VendorCode: "local_concurrency"})
		failN(t, b, 10, context.Canceled)
		require.Equal(t, BreakerClosed, b.State())
	})

	t.Run("failed probe reopens with a fresh openedAt", func(t *testing.T) {
		clk := clock.NewFake(t0)
		b := testBreaker(clk, nil)
		failN(t, b, 5, unavailable)
		clk.Advance(20 * time.Second)
		tk, ok := b.Allow()
		require.True(t, ok)
		tk.Done(unavailable)
		require.Equal(t, BreakerOpen, b.State())
		clk.Advance(19 * time.Second)
		_, ok = b.Allow()
		require.False(t, ok)
		clk.Advance(1 * time.Second)
		_, ok = b.Allow()
		require.True(t, ok)
	})

	t.Run("cancelled probe frees the slot and keeps HALF_OPEN", func(t *testing.T) {
		clk := clock.NewFake(t0)
		b := testBreaker(clk, nil)
		failN(t, b, 5, unavailable)
		clk.Advance(20 * time.Second)
		tk, ok := b.Allow()
		require.True(t, ok)
		tk.Done(context.Canceled)
		require.Equal(t, BreakerHalfOpen, b.State())
		tk2, ok := b.Allow()
		require.True(t, ok, "a new probe is allowed")
		tk2.Done(nil)
		require.Equal(t, BreakerClosed, b.State())
	})

	t.Run("success in CLOSED does not clear old failures", func(t *testing.T) {
		clk := clock.NewFake(t0)
		b := testBreaker(clk, nil)
		failN(t, b, 4, unavailable)
		tk, _ := b.Allow()
		tk.Done(nil)
		failN(t, b, 1, unavailable)
		require.Equal(t, BreakerOpen, b.State())
	})

	t.Run("success closing from half-open resets the window", func(t *testing.T) {
		clk := clock.NewFake(t0)
		b := testBreaker(clk, nil)
		failN(t, b, 5, unavailable)
		clk.Advance(20 * time.Second)
		tk, _ := b.Allow()
		tk.Done(nil)
		failN(t, b, 4, unavailable)
		require.Equal(t, BreakerClosed, b.State(), "the old failures are gone")
	})

	t.Run("onTransition fires exactly once per transition", func(t *testing.T) {
		clk := clock.NewFake(t0)
		var got [][2]BreakerState
		b := testBreaker(clk, func(from, to BreakerState) { got = append(got, [2]BreakerState{from, to}) })
		failN(t, b, 5, unavailable)
		clk.Advance(20 * time.Second)
		tk, _ := b.Allow()
		tk.Done(nil)
		require.Equal(t, [][2]BreakerState{{BreakerClosed, BreakerOpen}, {BreakerOpen, BreakerHalfOpen}, {BreakerHalfOpen, BreakerClosed}}, got)
	})
}

func TestBreakerConcurrent(_ *testing.T) {
	clk := clock.NewFake(t0)
	b := testBreaker(clk, func(_, _ BreakerState) {})
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				if tk, ok := b.Allow(); ok {
					if (i+j)%3 == 0 {
						tk.Done(unavailable)
					} else {
						tk.Done(nil)
					}
				}
				if j%20 == 0 {
					clk.Advance(5 * time.Second)
				}
				_ = b.State()
			}
		}(i)
	}
	wg.Wait()
}

func TestBreakerWouldAllowHasNoSideEffects(t *testing.T) {
	clk := clock.NewFake(t0)
	b := testBreaker(clk, nil)
	require.True(t, b.WouldAllow())
	failN(t, b, 5, unavailable)
	require.False(t, b.WouldAllow())
	clk.Advance(20 * time.Second)
	require.True(t, b.WouldAllow(), "after the open duration a probe would be admitted")
	require.Equal(t, BreakerOpen, b.State(), "peeking does not start the probe")
	tk, ok := b.Allow()
	require.True(t, ok)
	require.False(t, b.WouldAllow(), "the probe slot is taken")
	tk.Done(nil)
	require.True(t, b.WouldAllow())
}

func TestBreakerRecordCountsWithoutTicket(t *testing.T) {
	clk := clock.NewFake(t0)
	var to []BreakerState
	b := testBreaker(clk, func(_, s BreakerState) { to = append(to, s) })
	for i := 0; i < 4; i++ {
		b.Record(unavailable)
	}
	b.Record(nil)
	b.Record(&Error{Kind: KindCancelled})
	require.Equal(t, BreakerClosed, b.State())
	b.Record(unavailable)
	require.Equal(t, BreakerOpen, b.State())
	require.Equal(t, []BreakerState{BreakerOpen}, to)
	b.Record(unavailable) // already open: ignored
	require.Equal(t, []BreakerState{BreakerOpen}, to)
}
