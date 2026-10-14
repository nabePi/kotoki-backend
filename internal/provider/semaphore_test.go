package provider

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestSemaphoreFullReturnsLocalConcurrency(t *testing.T) {
	s := newSemaphoreSlot(1, 0)
	rel, err := s.Acquire(context.Background(), OpenAI, CapLLM)
	require.Nil(t, err)
	require.Equal(t, 1, s.InUse())
	_, err = s.Acquire(context.Background(), OpenAI, CapLLM)
	require.NotNil(t, err)
	require.Equal(t, "local_concurrency", err.VendorCode)
	require.Equal(t, KindRateLimited, err.Kind)
	require.True(t, err.Retryable)
	require.False(t, CountsForBreaker(err), "a full local semaphore must not trip the breaker")
	rel()
	require.Equal(t, 0, s.InUse())
	_, err = s.Acquire(context.Background(), OpenAI, CapLLM)
	require.Nil(t, err)
	require.Equal(t, 1, s.Limit())
}

func TestSemaphoreReleaseIsIdempotent(t *testing.T) {
	s := newSemaphoreSlot(1, 0)
	rel, err := s.Acquire(context.Background(), OpenAI, CapSTT)
	require.Nil(t, err)
	rel()
	rel() // a second release must not free someone else's slot
	r2, err := s.Acquire(context.Background(), OpenAI, CapSTT)
	require.Nil(t, err)
	_, err = s.Acquire(context.Background(), OpenAI, CapSTT)
	require.NotNil(t, err)
	r2()
}

func TestSemaphoreWaitsThenSucceeds(t *testing.T) {
	s := newSemaphoreSlot(1, time.Second)
	rel, _ := s.Acquire(context.Background(), OpenAI, CapLLM)
	go func() {
		time.Sleep(20 * time.Millisecond)
		rel()
	}()
	r2, err := s.Acquire(context.Background(), OpenAI, CapLLM)
	require.Nil(t, err)
	r2()
}

func TestSemaphoreWaitTimeout(t *testing.T) {
	s := newSemaphoreSlot(1, 30*time.Millisecond)
	_, _ = s.Acquire(context.Background(), OpenAI, CapLLM)
	start := time.Now()
	_, err := s.Acquire(context.Background(), OpenAI, CapLLM)
	require.NotNil(t, err)
	require.Equal(t, "local_concurrency", err.VendorCode)
	require.GreaterOrEqual(t, time.Since(start), 25*time.Millisecond)
}

func TestSemaphoreCancelledWhileWaiting(t *testing.T) {
	s := newSemaphoreSlot(1, time.Second)
	_, _ = s.Acquire(context.Background(), OpenAI, CapLLM)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	_, err := s.Acquire(ctx, OpenAI, CapLLM)
	require.NotNil(t, err)
	require.Equal(t, KindCancelled, err.Kind)
}
