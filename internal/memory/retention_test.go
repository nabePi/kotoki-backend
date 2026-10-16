package memory_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/memory"
	"github.com/nabePi/kotoki-backend/internal/platform/clock"
	"github.com/nabePi/kotoki-backend/internal/testutil/fakes"
)

// Spec 11.8.1: the retention window of every category, as the Applier stores it (not only TTLFor).
func TestRetention_TTLsStoredByApplier(t *testing.T) {
	f := newFixture(t)
	f.apply(cand(memory.CategoryProfileFact, "occupation", vec(0, 10)))
	f.apply(cand(memory.CategoryProfileFact, "plan:interview", vec(10, 20), func(c *memory.Candidate) {
		c.IsTemporary, c.ValidDays = true, ptr(45)
	}))
	f.apply(cand(memory.CategoryPreference, "likes:cats", vec(20, 30)))
	f.apply(cand(memory.CategoryLearningPattern, "pattern:particles", vec(30, 40)))
	f.apply(cand(memory.CategoryEpisodic, "episode:s1", vec(40, 50)))

	want := map[string]time.Duration{
		"occupation": days(730), "plan:interview": days(45), "likes:cats": days(365),
		"pattern:particles": days(180), "episode:s1": days(180),
	}
	got := 0
	for _, m := range f.store.All(f.user) {
		require.Equal(t, memory.StatusActive, m.Status)
		require.Equal(t, t0.Add(want[m.Key]), m.ExpiresAt, m.Key)
		got++
	}
	require.Equal(t, len(want), got)
}

// Spec 11.8.1: a superseded memory is kept 30 days; a user deletion leaves a 90 day tombstone.
func TestRetention_SupersededAndTombstoneWindows(t *testing.T) {
	f := newFixture(t)
	old := f.seed(memory.CategoryEpisodic, "episode:old", vec(0, 100))
	require.Equal(t, memory.ActionSuperseded, f.apply(cand(memory.CategoryEpisodic, "episode:new", vec(0, 90))))
	require.Equal(t, memory.StatusSuperseded, f.get(old.ID).Status)
	require.Equal(t, t0.Add(days(30)), f.get(old.ID).ExpiresAt)

	fact := f.seed(memory.CategoryProfileFact, "occupation", vec(50, 60))
	ver := fakes.NewMemVerRecorder()
	svc := memory.NewService(f.store, ver, fakes.NewMemoryProfile(), clock.NewFake(t0), nil)
	require.NoError(t, svc.Delete(context.Background(), f.user, fact.ID))
	var tomb int
	for _, m := range f.store.All(f.user) {
		if m.Status == memory.StatusDeleted {
			tomb++
			require.Equal(t, t0.Add(days(90)), m.ExpiresAt)
		}
	}
	require.Equal(t, 1, tomb)
}
