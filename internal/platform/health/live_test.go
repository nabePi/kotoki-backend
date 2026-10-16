package health_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/platform/health"
)

func TestLiveChecks_ReadyzTurns503WhenCheckFails(t *testing.T) {
	var fail atomic.Bool
	o := newOps(health.OpsOptions{LiveChecks: []health.Check{{Name: "amqp", Fn: func(context.Context) error {
		if fail.Load() {
			return errors.New("secret-broker-host:5672 refused")
		}
		return nil
	}}}})
	require.NoError(t, o.RunStartupChecks(context.Background()))

	code, body := get(t, o.Handler(), "/readyz")
	assert.Equal(t, 200, code)
	assert.Equal(t, `{"status":"ready"}`, body)

	fail.Store(true)
	code, body = get(t, o.Handler(), "/readyz")
	assert.Equal(t, 503, code)
	assert.Equal(t, `{"status":"not_ready"}`, body, "no error text leaks to the probe")

	fail.Store(false)
	code, body = get(t, o.Handler(), "/readyz")
	assert.Equal(t, 200, code)
	assert.Equal(t, `{"status":"ready"}`, body)
}

func TestLiveChecks_NotRunBeforeStartupAndDrainingWins(t *testing.T) {
	var calls atomic.Int32
	o := newOps(health.OpsOptions{LiveChecks: []health.Check{{Name: "x", Fn: func(context.Context) error {
		calls.Add(1)
		return errors.New("down")
	}}}})
	code, body := get(t, o.Handler(), "/readyz")
	assert.Equal(t, 503, code)
	assert.Equal(t, `{"status":"starting","checks":{}}`, body)
	assert.Zero(t, calls.Load(), "live checks wait for the startup checks")

	require.NoError(t, o.RunStartupChecks(context.Background()))
	o.SetDraining()
	code, body = get(t, o.Handler(), "/readyz")
	assert.Equal(t, 503, code)
	assert.Equal(t, `{"status":"draining"}`, body)
	assert.Zero(t, calls.Load(), "draining answers without running live checks")
}

func TestLiveChecks_TimeoutIsOneSecond(t *testing.T) {
	var remaining atomic.Int64
	o := newOps(health.OpsOptions{LiveChecks: []health.Check{{Name: "hang", Fn: func(ctx context.Context) error {
		if dl, ok := ctx.Deadline(); ok {
			remaining.Store(int64(time.Until(dl)))
		}
		<-ctx.Done()
		return ctx.Err()
	}}}})
	require.NoError(t, o.RunStartupChecks(context.Background()))
	start := time.Now()
	code, body := get(t, o.Handler(), "/readyz")
	took := time.Since(start)
	assert.Equal(t, 503, code)
	assert.Equal(t, `{"status":"not_ready"}`, body)
	assert.Greater(t, time.Duration(remaining.Load()), 500*time.Millisecond, "the check got a deadline of about 1 s")
	assert.LessOrEqual(t, time.Duration(remaining.Load()), time.Second)
	assert.GreaterOrEqual(t, took, 900*time.Millisecond, "a hanging check is waited for its timeout")
	assert.Less(t, took, 3*time.Second)
}

func TestLiveChecks_AllChecksEvaluatedOneFailureSuffices(t *testing.T) {
	o := newOps(health.OpsOptions{LiveChecks: []health.Check{
		{Name: "a", Fn: func(context.Context) error { return nil }},
		{Name: "b", Fn: func(context.Context) error { return errors.New("down") }},
	}})
	require.NoError(t, o.RunStartupChecks(context.Background()))
	code, _ := get(t, o.Handler(), "/readyz")
	assert.Equal(t, 503, code)
}
