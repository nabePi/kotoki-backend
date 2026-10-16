package main

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type recorder struct {
	mu    sync.Mutex
	order []string
}

func (r *recorder) add(s string) { r.mu.Lock(); r.order = append(r.order, s); r.mu.Unlock() }
func (r *recorder) got() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.order...)
}

func recordingLifecycle(r *recorder) (Lifecycle, *time.Duration) {
	var gotGrace time.Duration
	return Lifecycle{
		SetNotReady:   func() { r.add("ready_off") },
		StopScheduler: func(context.Context) { r.add("scheduler") },
		ShutdownConsumers: func(_ context.Context, g time.Duration) error {
			gotGrace = g
			r.add("consumers")
			return nil
		},
		StopPublisher: func(context.Context) error { r.add("publisher"); return nil },
		CloseAMQP:     func() error { r.add("amqp"); return nil },
		ClosePool:     func() { r.add("pool") },
		FlushOTel:     func(context.Context) error { r.add("otel"); return nil },
	}, &gotGrace
}

// Spec 9.7.7: ready off, scheduler, consumers, publisher, AMQP, pool, OTel.
func TestLifecycle_ShutdownOrder(t *testing.T) {
	r := &recorder{}
	l, gotGrace := recordingLifecycle(r)

	require.NoError(t, l.Shutdown(context.Background(), 25*time.Second))

	assert.Equal(t, []string{"ready_off", "scheduler", "consumers", "publisher", "amqp", "pool", "otel"}, r.got())
	assert.LessOrEqual(t, *gotGrace, 25*time.Second)
	assert.Greater(t, *gotGrace, 24*time.Second, "an instant scheduler leaves the consumers the whole grace")
}

// Scheduler and consumers share ONE grace window (9.7.7 steps 2 to 5): time the scheduler spent
// waiting for its running jobs is taken from what the handlers get.
func TestLifecycle_GraceIsOneWindow(t *testing.T) {
	r := &recorder{}
	l, gotGrace := recordingLifecycle(r)
	l.StopScheduler = func(context.Context) { r.add("scheduler"); time.Sleep(600 * time.Millisecond) }

	require.NoError(t, l.Shutdown(context.Background(), 2*time.Second))

	assert.LessOrEqual(t, *gotGrace, 1500*time.Millisecond)
	assert.Greater(t, *gotGrace, 500*time.Millisecond)
}

// Every step gets a context that outlives the grace by a short slack: Consumer.Shutdown cancels the
// handlers when the grace ends and must still be able to wait for their nacks.
func TestLifecycle_StepsGetSlackBeyondGrace(t *testing.T) {
	var dl time.Time
	l := Lifecycle{ShutdownConsumers: func(ctx context.Context, _ time.Duration) error {
		dl, _ = ctx.Deadline()
		return nil
	}}
	start := time.Now()
	require.NoError(t, l.Shutdown(context.Background(), 2*time.Second))
	assert.GreaterOrEqual(t, dl.Sub(start), 2*time.Second+shutdownSlack-500*time.Millisecond)
	assert.LessOrEqual(t, dl.Sub(start), 2*time.Second+shutdownSlack+500*time.Millisecond)
}

func TestLifecycle_StepErrorDoesNotSkipTheRest(t *testing.T) {
	r := &recorder{}
	l, _ := recordingLifecycle(r)
	first, second := errors.New("consumers stuck"), errors.New("amqp close")
	l.ShutdownConsumers = func(context.Context, time.Duration) error { r.add("consumers"); return first }
	l.CloseAMQP = func() error { r.add("amqp"); return second }

	err := l.Shutdown(context.Background(), time.Second)

	assert.Equal(t, []string{"ready_off", "scheduler", "consumers", "publisher", "amqp", "pool", "otel"}, r.got())
	require.ErrorIs(t, err, first)
	assert.NotErrorIs(t, err, second, "the first error is returned")
}

func TestLifecycle_NilStepsAreSkipped(t *testing.T) {
	require.NoError(t, Lifecycle{}.Shutdown(context.Background(), time.Second))
}
