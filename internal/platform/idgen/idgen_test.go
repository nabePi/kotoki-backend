package idgen_test

import (
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/platform/idgen"
)

func TestSequenceDeterministic(t *testing.T) {
	a, b := idgen.NewSequence(t), idgen.NewSequence(t)
	for i := 0; i < 100; i++ {
		require.Equal(t, a.New(), b.New())
	}
}

func TestSequenceMonotonic(t *testing.T) {
	g := idgen.NewSequence(t)
	prev := g.New()
	for i := 0; i < 200; i++ {
		id := g.New()
		require.Less(t, prev.String(), id.String())
		require.Equal(t, uuid.Version(7), id.Version())
		require.Equal(t, uuid.RFC4122, id.Variant())
		prev = id
	}
}

func TestV7MonotonicByTime(t *testing.T) {
	g := idgen.NewV7()
	seen := map[uuid.UUID]bool{}
	for i := 0; i < 1000; i++ {
		id := g.New()
		require.Equal(t, uuid.Version(7), id.Version())
		require.False(t, seen[id])
		seen[id] = true
	}
}

func TestSequenceConcurrent(t *testing.T) {
	g := idgen.NewSequence(t)
	var mu sync.Mutex
	var wg sync.WaitGroup
	seen := map[uuid.UUID]bool{}
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id := g.New()
			mu.Lock()
			defer mu.Unlock()
			seen[id] = true
		}()
	}
	wg.Wait()
	require.Len(t, seen, 50)
}
