package fakes_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/events"
	"github.com/nabePi/kotoki-backend/internal/testutil/fakes"
)

func TestProcessed(t *testing.T) {
	ctx := context.Background()
	p := fakes.NewProcessed()
	env := events.Envelope{EventID: uuid.New(), EventType: events.SessionCompleted}

	got, err := p.IsProcessed(ctx, events.QueueMemory, env.EventID)
	require.NoError(t, err)
	require.False(t, got)

	require.NoError(t, p.MarkProcessed(ctx, events.QueueMemory, env))
	require.ErrorIs(t, p.MarkProcessed(ctx, events.QueueMemory, env), events.ErrAlreadyProcessed)
	require.NoError(t, p.MarkProcessed(ctx, events.QueueSummary, env), "other consumer is independent")
	got, _ = p.IsProcessed(ctx, events.QueueMemory, env.EventID)
	require.True(t, got)
	require.Equal(t, 2, p.Len())

	p.Forget(events.QueueMemory, env.EventID)
	got, _ = p.IsProcessed(ctx, events.QueueMemory, env.EventID)
	require.False(t, got)

	boom := errors.New("boom")
	p.FailNext("MarkProcessed", boom)
	require.ErrorIs(t, p.MarkProcessed(ctx, events.QueueMemory, env), boom)
	p.FailNext("IsProcessed", boom)
	_, err = p.IsProcessed(ctx, events.QueueMemory, env.EventID)
	require.ErrorIs(t, err, boom)
}

func TestProcessed_ConcurrentExactlyOneWins(t *testing.T) {
	p := fakes.NewProcessed()
	env := events.Envelope{EventID: uuid.New()}
	var wg sync.WaitGroup
	var mu sync.Mutex
	wins := 0
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if p.MarkProcessed(context.Background(), events.QueueEvaluation, env) == nil {
				mu.Lock()
				wins++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	require.Equal(t, 1, wins)
}
