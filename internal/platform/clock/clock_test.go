package clock_test

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/platform/clock"
)

func TestFakeClockAdvance(t *testing.T) {
	t0 := time.Date(2026, 10, 13, 8, 0, 0, 0, time.UTC)
	f := clock.NewFake(t0)
	f.Advance(90 * time.Second)
	require.True(t, f.Now().Equal(t0.Add(90*time.Second)))
	require.Equal(t, time.UTC, f.Now().Location())
	f.Set(t0)
	require.True(t, f.Now().Equal(t0))
}

func TestFakeClockConcurrent(t *testing.T) {
	t0 := time.Date(2026, 10, 13, 8, 0, 0, 0, time.UTC)
	f := clock.NewFake(t0)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			f.Advance(time.Second)
			_ = f.Now()
		}()
	}
	wg.Wait()
	require.True(t, f.Now().Equal(t0.Add(50*time.Second)))
}

func TestSystemUTC(t *testing.T) {
	var c clock.Clock = clock.System{}
	require.Equal(t, time.UTC, c.Now().Location())
}
