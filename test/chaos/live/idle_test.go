package live_test

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/test/chaos/live"
)

func counter(vals ...int) (func(context.Context) (int, error), *int) {
	i := 0
	return func(context.Context) (int, error) {
		v := vals[min(i, len(vals)-1)]
		i++
		return v, nil
	}, &i
}

func TestWaitIdle_ReturnsWhenNothingIsOpen(t *testing.T) {
	count, calls := counter(7, 3, 1, 0)
	var out bytes.Buffer
	w := live.IdleWaiter{Count: count, Poll: time.Millisecond, Timeout: 5 * time.Second, Out: &out}
	require.NoError(t, w.Wait(t.Context()))
	require.Equal(t, 4, *calls)
	require.Contains(t, out.String(), "7 session")
}

func TestWaitIdle_TimesOutWithTheCount(t *testing.T) {
	count, _ := counter(2)
	w := live.IdleWaiter{Count: count, Poll: time.Millisecond, Timeout: 50 * time.Millisecond}
	err := w.Wait(t.Context())
	require.ErrorContains(t, err, "2 session")
}

func TestWaitIdle_CountErrorStopsTheWait(t *testing.T) {
	w := live.IdleWaiter{Count: func(context.Context) (int, error) { return 0, errors.New("db down") }, Poll: time.Millisecond, Timeout: time.Second}
	require.ErrorContains(t, w.Wait(t.Context()), "db down")
}

func TestWaitIdle_ContextCancel(t *testing.T) {
	count, _ := counter(5)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	require.Error(t, live.IdleWaiter{Count: count, Poll: time.Hour, Timeout: time.Hour}.Wait(ctx))
}
