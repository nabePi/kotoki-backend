package kotokiclient

import (
	"context"
	"math/rand"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/platform/clock"
)

var t0 = time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC)

// lateSleeper advances a fake clock by d plus a random lateness in [0, maxLate].
func lateSleeper(clk *clock.Fake, rng *rand.Rand, maxLate time.Duration) func(context.Context, time.Duration) error {
	return func(_ context.Context, d time.Duration) error {
		late := time.Duration(0)
		if maxLate > 0 {
			late = time.Duration(rng.Int63n(int64(maxLate) + 1))
		}
		clk.Advance(d + late)
		return nil
	}
}

func TestPacerNoDrift(t *testing.T) {
	const maxLate = 3 * time.Millisecond
	clk := clock.NewFake(t0)
	p := NewPacer(t0, clk.Now, lateSleeper(clk, rand.New(rand.NewSource(1)), maxLate))
	for k := 1; k <= 1000; k++ {
		require.NoError(t, p.Wait(context.Background()))
		slot := t0.Add(time.Duration(k) * FrameInterval)
		got := clk.Now()
		require.False(t, got.Before(slot), "wait %d returned before its slot", k)
		// Lateness is that of one sleep only; a pacer that sleeps 20 ms relative to now would
		// accumulate ~1.5 ms per wait and be ~1.5 s late by the end.
		require.LessOrEqual(t, got.Sub(slot), maxLate, "wait %d drifted", k)
	}
}

func TestPacerExactSchedule(t *testing.T) {
	clk := clock.NewFake(t0)
	p := NewPacer(t0, clk.Now, lateSleeper(clk, nil, 0))
	for k := 1; k <= 1000; k++ {
		require.NoError(t, p.Wait(context.Background()))
	}
	require.True(t, clk.Now().Equal(t0.Add(20*time.Second)), "1000 waits end at start+20s, got %s", clk.Now().Sub(t0))
}

func TestPacerCatchesUpWithoutSleeping(t *testing.T) {
	clk := clock.NewFake(t0)
	slept := 0
	p := NewPacer(t0, clk.Now, func(_ context.Context, d time.Duration) error {
		slept++
		clk.Advance(d)
		return nil
	})
	clk.Advance(105 * time.Millisecond) // the caller stalled: slots 1..5 are already due
	for k := 1; k <= 5; k++ {
		require.NoError(t, p.Wait(context.Background()))
	}
	require.Zero(t, slept, "no sleep while behind schedule")
	require.NoError(t, p.Wait(context.Background())) // slot 6 = 120 ms, now = 105 ms
	require.Equal(t, 1, slept)
	require.True(t, clk.Now().Equal(t0.Add(120*time.Millisecond)))
}

func TestPacerStopsOnContext(t *testing.T) {
	clk := clock.NewFake(t0)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p := NewPacer(t0, clk.Now, func(context.Context, time.Duration) error {
		t.Fatal("sleep must not run on a cancelled context")
		return nil
	})
	require.ErrorIs(t, p.Wait(ctx), context.Canceled)

	// A sleep that is interrupted reports the context error.
	ctx2, cancel2 := context.WithCancel(context.Background())
	p2 := NewPacer(t0, clk.Now, func(c context.Context, _ time.Duration) error {
		cancel2()
		return c.Err()
	})
	require.ErrorIs(t, p2.Wait(ctx2), context.Canceled)
}

func TestSleepContext(t *testing.T) {
	require.NoError(t, SleepContext(context.Background(), time.Millisecond))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.ErrorIs(t, SleepContext(ctx, time.Hour), context.Canceled)
}

func TestNewClockPacerUsesTheClock(t *testing.T) {
	clk := clock.NewFake(t0)
	clk.Advance(time.Hour) // far past every slot: Wait must not sleep (it would take an hour)
	p := NewClockPacer(t0, clk)
	require.NoError(t, p.Wait(context.Background()))
}
