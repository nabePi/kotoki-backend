package fakes_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/events"
	"github.com/nabePi/kotoki-backend/internal/memory"
	"github.com/nabePi/kotoki-backend/internal/testutil/fakes"
	"github.com/nabePi/kotoki-backend/internal/testutil/repotest"
)

var memNow = time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC)

func memRow(user uuid.UUID, n byte, cat memory.Category, key string, mod ...func(*memory.Memory)) memory.Memory {
	m := memory.Memory{
		ID: uuid.UUID{15: n}, UserID: user, Category: cat, Key: key, Content: "内容", DisplayText: "isi",
		EvidenceType: memory.EvidenceExplicit, Confidence: 0.9, Importance: 0.5, ObservationCount: 1,
		Embedding: []float32{1, 0}, Status: memory.StatusActive,
		FirstObservedAt: memNow, LastObservedAt: memNow, ExpiresAt: memNow.Add(time.Hour), CreatedAt: memNow, UpdatedAt: memNow,
	}
	for _, f := range mod {
		f(&m)
	}
	return m
}

func inTx(t *testing.T, s *fakes.MemoryStore, fn func(r memory.Repository) error) error {
	t.Helper()
	return s.WithinTx(context.Background(), func(_ context.Context, tx memory.Tx) error { return fn(tx.Memories()) })
}

func TestMemoryStore_ActiveSlotIsUnique(t *testing.T) {
	s, u := fakes.NewMemoryStore(), uuid.UUID{0: 1}
	require.NoError(t, inTx(t, s, func(r memory.Repository) error {
		return r.Insert(context.Background(), u, memRow(u, 1, memory.CategoryProfileFact, "occupation"))
	}))
	err := inTx(t, s, func(r memory.Repository) error {
		return r.Insert(context.Background(), u, memRow(u, 2, memory.CategoryProfileFact, "occupation"))
	})
	require.Error(t, err, "a second ACTIVE row of the slot violates ux_memories_active_slot")
	// another user, another category and a SUPERSEDED row are fine
	other := uuid.UUID{0: 2}
	require.NoError(t, inTx(t, s, func(r memory.Repository) error {
		ctx := context.Background()
		if err := r.Insert(ctx, other, memRow(other, 3, memory.CategoryProfileFact, "occupation")); err != nil {
			return err
		}
		if err := r.Insert(ctx, u, memRow(u, 4, memory.CategoryPreference, "occupation")); err != nil {
			return err
		}
		return r.Insert(ctx, u, memRow(u, 5, memory.CategoryProfileFact, "occupation", func(m *memory.Memory) { m.Status = memory.StatusSuperseded }))
	}))
	// an insert for a row that carries another user is refused
	require.Error(t, inTx(t, s, func(r memory.Repository) error {
		return r.Insert(context.Background(), u, memRow(other, 6, memory.CategoryProfileFact, "x"))
	}))
}

func TestMemoryStore_DeleteLeavesTombstoneAndRemovesChain(t *testing.T) {
	s, u := fakes.NewMemoryStore(), uuid.UUID{0: 1}
	cur := memRow(u, 3, memory.CategoryProfileFact, "employer")
	mid := memRow(u, 2, memory.CategoryProfileFact, "employer", func(m *memory.Memory) {
		m.Status, m.SupersededBy = memory.StatusSuperseded, &cur.ID
	})
	old := memRow(u, 1, memory.CategoryProfileFact, "employer", func(m *memory.Memory) {
		m.Status, m.SupersededBy = memory.StatusSuperseded, &mid.ID
	})
	unrelated := memRow(u, 4, memory.CategoryProfileFact, "occupation")
	for _, m := range []memory.Memory{cur, mid, old, unrelated} {
		s.Put(m)
	}
	var ok bool
	require.NoError(t, inTx(t, s, func(r memory.Repository) (err error) {
		ok, err = r.Delete(context.Background(), u, cur.ID, memNow)
		return err
	}))
	require.True(t, ok)
	rows := s.All(u)
	require.Len(t, rows, 2, "chain and row gone; the unrelated row and the tombstone remain")
	var tomb memory.Memory
	for _, m := range rows {
		if m.Status == memory.StatusDeleted {
			tomb = m
		}
	}
	require.Equal(t, memory.CategoryProfileFact, tomb.Category)
	require.Equal(t, "employer", tomb.Key)
	require.Empty(t, tomb.Content)
	require.Empty(t, tomb.DisplayText)
	require.Nil(t, tomb.Embedding)
	require.Equal(t, memory.EvidenceUserDeleted, tomb.EvidenceType)
	require.Equal(t, memNow.Add(90*24*time.Hour), tomb.ExpiresAt)

	// deleting again (no longer ACTIVE) reports not found; a re-created slot refreshes the same tombstone
	require.NoError(t, inTx(t, s, func(r memory.Repository) (err error) {
		ok, err = r.Delete(context.Background(), u, cur.ID, memNow)
		return err
	}))
	require.False(t, ok)
	later := memNow.Add(24 * time.Hour)
	s.Put(memRow(u, 7, memory.CategoryProfileFact, "employer"))
	require.NoError(t, inTx(t, s, func(r memory.Repository) (err error) {
		ok, err = r.Delete(context.Background(), u, uuid.UUID{15: 7}, later)
		return err
	}))
	require.True(t, ok)
	tombs := 0
	for _, m := range s.All(u) {
		if m.Status == memory.StatusDeleted {
			tombs++
			require.Equal(t, later.Add(90*24*time.Hour), m.ExpiresAt)
		}
	}
	require.Equal(t, 1, tombs, "one tombstone per slot")
}

func TestMemoryStore_DeleteEpisodicLeavesNoTombstone(t *testing.T) {
	s, u := fakes.NewMemoryStore(), uuid.UUID{0: 1}
	s.Put(memRow(u, 1, memory.CategoryEpisodic, "episode:x"))
	var ok bool
	require.NoError(t, inTx(t, s, func(r memory.Repository) (err error) {
		ok, err = r.Delete(context.Background(), u, uuid.UUID{15: 1}, memNow)
		return err
	}))
	require.True(t, ok)
	require.Empty(t, s.All(u))
}

func TestMemoryStore_UserScoping(t *testing.T) {
	s, a, b := fakes.NewMemoryStore(), uuid.UUID{0: 1}, uuid.UUID{0: 2}
	s.Put(memRow(a, 1, memory.CategoryProfileFact, "occupation"))
	s.Put(memRow(a, 2, memory.CategoryEpisodic, "episode:a"))
	s.Put(memRow(b, 3, memory.CategoryProfileFact, "occupation"))
	ctx := context.Background()
	require.NoError(t, inTx(t, s, func(r memory.Repository) error {
		// B cannot see, reinforce, supersede or delete A's row
		got, err := r.GetActiveSlotForUpdate(ctx, b, memory.CategoryPreference, "occupation")
		require.NoError(t, err)
		require.Nil(t, got)
		n, err := r.FindNearestActive(ctx, b, memory.CategoryEpisodic, []float32{1, 0})
		require.NoError(t, err)
		require.Nil(t, n, "identical embedding of another user is invisible")
		require.ErrorIs(t, r.Reinforce(ctx, b, uuid.UUID{15: 1}, memory.ReinforceInput{Now: memNow}), memory.ErrNotFound)
		require.ErrorIs(t, r.Supersede(ctx, b, uuid.UUID{15: 1}, uuid.UUID{15: 9}, memNow, memNow), memory.ErrNotFound)
		ok, err := r.Delete(ctx, b, uuid.UUID{15: 1}, memNow)
		require.NoError(t, err)
		require.False(t, ok)
		c, err := r.CountActive(ctx, b, memory.CapEpisodic)
		require.NoError(t, err)
		require.Zero(t, c)
		core, err := r.ListCore(ctx, b, memNow)
		require.NoError(t, err)
		require.Len(t, core, 1)
		require.Equal(t, uuid.UUID{15: 3}, core[0].ID)
		hits, err := r.SearchEpisodic(ctx, b, []float32{1, 0}, 0.5, 3, nil, memNow)
		require.NoError(t, err)
		require.Empty(t, hits)
		page, err := r.List(ctx, b, nil, nil, 10, memNow)
		require.NoError(t, err)
		require.Len(t, page, 1)
		require.NoError(t, r.DeleteLowest(ctx, b, memory.CapEpisodic, 5))
		return nil
	}))
	require.Len(t, s.All(a), 2, "A is untouched")
	require.NoError(t, inTx(t, s, func(r memory.Repository) error { return r.DeleteAll(ctx, b) }))
	require.Empty(t, s.All(b))
	require.Len(t, s.All(a), 2)
}

func TestMemoryStore_Reads(t *testing.T) {
	s, u := fakes.NewMemoryStore(), uuid.UUID{0: 1}
	s.Put(memRow(u, 1, memory.CategoryPreference, "pref:formality", func(m *memory.Memory) { m.LastObservedAt = memNow.Add(-time.Hour) }))
	s.Put(memRow(u, 2, memory.CategoryProfileFact, "occupation", func(m *memory.Memory) { m.LastObservedAt = memNow.Add(-2 * time.Hour) }))
	s.Put(memRow(u, 3, memory.CategoryProfileFact, "employer"))
	s.Put(memRow(u, 4, memory.CategoryProfileFact, "expired", func(m *memory.Memory) { m.ExpiresAt = memNow }))
	s.Put(memRow(u, 5, memory.CategoryProfileFact, "old", func(m *memory.Memory) { m.Status = memory.StatusSuperseded }))
	sess := uuid.UUID{0: 9}
	s.Put(memRow(u, 6, memory.CategoryEpisodic, "episode:6", func(m *memory.Memory) { m.Embedding = []float32{1, 0.1} }))
	s.Put(memRow(u, 7, memory.CategoryEpisodic, "episode:7", func(m *memory.Memory) { m.Embedding = []float32{1, 1}; m.SourceSessionID = &sess }))
	s.Put(memRow(u, 8, memory.CategoryEpisodic, "episode:8", func(m *memory.Memory) { m.Embedding = []float32{0, 1} }))
	ctx, rd := context.Background(), s.Reader()

	core, err := rd.ListCore(ctx, u, memNow)
	require.NoError(t, err)
	require.Equal(t, []uuid.UUID{{15: 3}, {15: 1}, {15: 2}}, []uuid.UUID{core[0].ID, core[1].ID, core[2].ID}, "newest first, no episodes, expired or superseded")
	require.Len(t, core, 3)

	hits, err := rd.SearchEpisodic(ctx, u, []float32{1, 0}, 0.7, 3, nil, memNow)
	require.NoError(t, err)
	require.Len(t, hits, 2, "cosine 0.995 and 0.707; the orthogonal one is below the threshold")
	require.Equal(t, uuid.UUID{15: 6}, hits[0].ID)
	require.Greater(t, hits[0].Similarity, hits[1].Similarity)
	hits, err = rd.SearchEpisodic(ctx, u, []float32{1, 0}, 0.7, 3, &sess, memNow)
	require.NoError(t, err)
	require.Len(t, hits, 1, "the session being run is excluded")
	hits, err = rd.SearchEpisodic(ctx, u, []float32{1, 0}, 0.7, 1, nil, memNow)
	require.NoError(t, err)
	require.Len(t, hits, 1, "k")

	all, err := rd.List(ctx, u, nil, nil, 100, memNow)
	require.NoError(t, err)
	var ids []uuid.UUID
	for _, m := range all {
		ids = append(ids, m.ID)
	}
	require.Equal(t, []uuid.UUID{{15: 6}, {15: 7}, {15: 8}, {15: 1}, {15: 3}, {15: 2}}, ids, "category (as text, like the SQL), then newest, then id; ACTIVE and unexpired only")

	// keyset paging without gaps or repeats
	var seen []uuid.UUID
	var cur *memory.ListCursor
	for {
		page, err := rd.List(ctx, u, nil, cur, 2, memNow)
		require.NoError(t, err)
		for _, m := range page {
			seen = append(seen, m.ID)
		}
		if len(page) < 2 {
			break
		}
		last := page[len(page)-1]
		cur = &memory.ListCursor{Category: last.Category, LastObservedAt: last.LastObservedAt, ID: last.ID}
	}
	require.Equal(t, ids, seen)
	cat := memory.CategoryEpisodic
	only, err := rd.List(ctx, u, &cat, nil, 100, memNow)
	require.NoError(t, err)
	require.Len(t, only, 3)
}

func TestMemoryStore_RollbackRestoresRowsOpsAndProcessed(t *testing.T) {
	s, u := fakes.NewMemoryStore(), uuid.UUID{0: 1}
	s.Put(memRow(u, 1, memory.CategoryProfileFact, "occupation"))
	boom := errors.New("boom")
	ev := events.Envelope{EventID: uuid.UUID{0: 7}}
	err := s.WithinTx(context.Background(), func(ctx context.Context, tx memory.Tx) error {
		require.NoError(t, tx.Processed().MarkProcessed(ctx, "memory", ev))
		require.NoError(t, tx.Outbox().Enqueue(ctx, events.Event{EventType: "x"}))
		require.NoError(t, tx.Memories().Insert(ctx, u, memRow(u, 2, memory.CategoryPreference, "pref:formality")))
		_, err := tx.Memories().Delete(ctx, u, uuid.UUID{15: 1}, memNow)
		require.NoError(t, err)
		return boom
	})
	require.ErrorIs(t, err, boom)
	require.Len(t, s.All(u), 1)
	_, still := s.Get(u, uuid.UUID{15: 1})
	require.True(t, still)
	require.Empty(t, s.Ops())
	require.Zero(t, s.Processed.Len())
	require.Zero(t, s.Outbox.Len())

	require.NoError(t, s.WithinTx(context.Background(), func(ctx context.Context, tx memory.Tx) error {
		return tx.Processed().MarkProcessed(ctx, "memory", ev)
	}))
	require.Equal(t, 1, s.Processed.Len(), "a committed transaction keeps its row")
}

func TestFakeMemoryStore_RepoSuite(t *testing.T) {
	repotest.MemoryRepoSuite(t, func(*testing.T) repotest.MemoryHarness {
		s := fakes.NewMemoryStore()
		return repotest.MemoryHarness{
			Store:           s,
			NewUser:         func(*testing.T) uuid.UUID { return uuid.Must(uuid.NewV7()) },
			NewSession:      func(*testing.T, uuid.UUID) uuid.UUID { return uuid.Must(uuid.NewV7()) },
			PromptVersionID: uuid.Must(uuid.NewV7()),
			Dump:            func(_ *testing.T, userID uuid.UUID) []memory.Memory { return s.All(userID) },
		}
	})
}
