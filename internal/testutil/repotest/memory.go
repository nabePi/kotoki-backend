package repotest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/memory"
)

// memoryDim is the width of memories.embedding; every vector of the suite has it, so the same
// cases run against the fake and against pgvector.
const memoryDim = 1536

// MemoryHarness is one fresh memory store plus what it needs to accept rows: PostgreSQL requires
// real users, sessions and a prompt version (foreign keys), a fake does not.
type MemoryHarness struct {
	Store memory.Store
	// NewUser returns a fresh user.
	NewUser func(t *testing.T) uuid.UUID
	// NewSession returns a session of userID.
	NewSession func(t *testing.T, userID uuid.UUID) uuid.UUID
	// PromptVersionID is a prompt version that exists.
	PromptVersionID uuid.UUID
	// Dump returns every memories row of userID whatever its status (embedding not required).
	Dump func(t *testing.T, userID uuid.UUID) []memory.Memory
}

var memAt = time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC)

// vec is a unit-length-direction vector (x, y, 0, ...) of the width of the column.
func vec(x, y float32) []float32 {
	v := make([]float32, memoryDim)
	v[0], v[1] = x, y
	return v
}

// mid is a deterministic memory id; ids order like n.
func mid(n int) uuid.UUID { return uuid.UUID{14: byte(n >> 8 & 0xff), 15: byte(n & 0xff)} }

func mkMem(user uuid.UUID, n int, cat memory.Category, key string, mod ...func(*memory.Memory)) memory.Memory {
	m := memory.Memory{
		ID: mid(n), UserID: user, Category: cat, Key: key, Content: "内容", DisplayText: "isi",
		EvidenceType: memory.EvidenceExplicit, Confidence: 0.9, Importance: 0.5, ObservationCount: 1,
		Embedding: vec(1, 0), EmbeddingModel: "test-embedding-1", Status: memory.StatusActive,
		FirstObservedAt: memAt, LastObservedAt: memAt, ExpiresAt: memAt.Add(24 * time.Hour), CreatedAt: memAt, UpdatedAt: memAt,
	}
	for _, f := range mod {
		f(&m)
	}
	if m.FirstObservedAt.After(m.LastObservedAt) { // chk_memories_observed
		m.FirstObservedAt = m.LastObservedAt
	}
	return m
}

func (h MemoryHarness) tx(t *testing.T, fn func(ctx context.Context, r memory.Repository)) {
	t.Helper()
	require.NoError(t, h.Store.WithinTx(context.Background(), func(ctx context.Context, tx memory.Tx) error {
		fn(ctx, tx.Memories())
		return nil
	}))
}

func (h MemoryHarness) put(t *testing.T, ms ...memory.Memory) {
	t.Helper()
	h.tx(t, func(ctx context.Context, r memory.Repository) {
		for _, m := range ms {
			require.NoError(t, r.Insert(ctx, m.UserID, m))
		}
	})
}

func (h MemoryHarness) row(t *testing.T, userID, id uuid.UUID) (memory.Memory, bool) {
	t.Helper()
	for _, m := range h.Dump(t, userID) {
		if m.ID == id {
			return m, true
		}
	}
	return memory.Memory{}, false
}

func (h MemoryHarness) mustRow(t *testing.T, userID, id uuid.UUID) memory.Memory {
	t.Helper()
	m, ok := h.row(t, userID, id)
	require.True(t, ok, "row %s of user %s exists", id, userID)
	return m
}

func (h MemoryHarness) ids(t *testing.T, userID uuid.UUID) []uuid.UUID {
	t.Helper()
	var out []uuid.UUID
	for _, m := range h.Dump(t, userID) {
		out = append(out, m.ID)
	}
	return out
}

func idsOf[T any](rows []T, id func(T) uuid.UUID) []uuid.UUID {
	out := make([]uuid.UUID, 0, len(rows))
	for _, r := range rows {
		out = append(out, id(r))
	}
	return out
}

// MemoryRepoSuite is the contract of the memory repository, run against the in-memory fake and
// PostgreSQL alike (spec 11.6 to 11.10). The isolation cases hold every method to the rule that a
// user never sees or changes the rows of another.
func MemoryRepoSuite(t *testing.T, mk func(t *testing.T) MemoryHarness) {
	Each(t, mk,
		Case[MemoryHarness]{Name: "InsertRoundTripAndSlotLookup", Run: memInsertRoundTrip},
		Case[MemoryHarness]{Name: "ActiveAndTombstoneSlotsAreUnique", Run: memSlotUnique},
		Case[MemoryHarness]{Name: "ReinforceFormulaAndColumns", Run: memReinforce},
		Case[MemoryHarness]{Name: "ReinforceOfMissingOrInactiveIsNotFound", Run: memReinforceNotFound},
		Case[MemoryHarness]{Name: "SupersedeThenInsertInOneTransaction", Run: memSupersede},
		Case[MemoryHarness]{Name: "DeleteRemovesChainAndLeavesTombstone", Run: memDeleteChain},
		Case[MemoryHarness]{Name: "DeleteUpsertsTheTombstoneOfTheSlot", Run: memDeleteUpsert},
		Case[MemoryHarness]{Name: "DeleteOfEpisodeLeavesNoTombstone", Run: memDeleteEpisodic},
		Case[MemoryHarness]{Name: "DeleteOfMissingOrInactiveIsFalse", Run: memDeleteMissing},
		Case[MemoryHarness]{Name: "TombstoneIsActiveUntilItExpires", Run: memTombstoneExpiry},
		Case[MemoryHarness]{Name: "CountActivePerCapGroup", Run: memCountActive},
		Case[MemoryHarness]{Name: "DeleteLowestOrdersByScoreThenAgeThenID", Run: memDeleteLowestScore},
		Case[MemoryHarness]{Name: "DeleteLowestEpisodicGoesByAge", Run: memDeleteLowestEpisodic},
		Case[MemoryHarness]{Name: "DeleteLowestFullTieGoesByID", Run: memDeleteLowestTie},
		Case[MemoryHarness]{Name: "DeleteLowestEdgeCases", Run: memDeleteLowestEdges},
		Case[MemoryHarness]{Name: "ListCoreFiltersAndOrders", Run: memListCore},
		Case[MemoryHarness]{Name: "SearchEpisodicThresholdExcludeAndK", Run: memSearchEpisodic},
		Case[MemoryHarness]{Name: "FindNearestActiveRanksWithinCategory", Run: memFindNearest},
		Case[MemoryHarness]{Name: "ListFiltersOrdersAndPages", Run: memList},
		Case[MemoryHarness]{Name: "DeleteAllKeepsOtherUsers", Run: memDeleteAll},
		Case[MemoryHarness]{Name: "IsolationReadsNeverCrossUsers", Run: memIsolationReads},
		Case[MemoryHarness]{Name: "IsolationWritesNeverReachOtherUsers", Run: memIsolationWrites},
		Case[MemoryHarness]{Name: "IsolationCapEvictionStaysInUser", Run: memIsolationCap},
		Case[MemoryHarness]{Name: "IsolationTombstonesStayInUser", Run: memIsolationTombstone},
		Case[MemoryHarness]{Name: "TransactionRollsBackAndCommitIsVisible", Run: memRollback},
	)
}

func memInsertRoundTrip(t *testing.T, h MemoryHarness) {
	u := h.NewUser(t)
	sess, turn := h.NewSession(t, u), uuid.UUID{0: 7}
	pv := h.PromptVersionID
	m := mkMem(u, 1, memory.CategoryProfileFact, "occupation", func(m *memory.Memory) {
		m.SourceSessionID, m.SourceTurnIDs, m.ExtractionPromptVersionID = &sess, []uuid.UUID{turn}, &pv
		m.Content, m.DisplayText, m.Confidence, m.Importance, m.ObservationCount = "エンジニア", "Insinyur", 0.875, 0.625, 3
		m.Embedding = vec(0.25, 0.5)
		m.ExpiresAt = memAt.Add(730 * 24 * time.Hour)
	})
	h.put(t, m)

	got := h.mustRow(t, u, m.ID)
	require.Equal(t, memory.CategoryProfileFact, got.Category)
	require.Equal(t, "occupation", got.Key)
	require.Equal(t, "エンジニア", got.Content)
	require.Equal(t, "Insinyur", got.DisplayText)
	require.Equal(t, memory.EvidenceExplicit, got.EvidenceType)
	require.InDelta(t, 0.875, got.Confidence, 1e-9)
	require.InDelta(t, 0.625, got.Importance, 1e-9)
	require.Equal(t, 3, got.ObservationCount)
	require.Equal(t, &sess, got.SourceSessionID)
	require.Equal(t, []uuid.UUID{turn}, got.SourceTurnIDs)
	require.Equal(t, &pv, got.ExtractionPromptVersionID)
	require.Equal(t, memory.StatusActive, got.Status)
	require.True(t, memAt.Equal(got.FirstObservedAt) && memAt.Equal(got.LastObservedAt))
	require.True(t, m.ExpiresAt.Equal(got.ExpiresAt))

	h.tx(t, func(ctx context.Context, r memory.Repository) {
		slot, err := r.GetActiveSlotForUpdate(ctx, u, memory.CategoryProfileFact, "occupation")
		require.NoError(t, err)
		require.NotNil(t, slot)
		require.Equal(t, m.ID, slot.ID)
		require.Equal(t, m.Embedding, slot.Embedding, "the 1536-wide vector round-trips exactly")
		require.Equal(t, "test-embedding-1", slot.EmbeddingModel)
		for _, miss := range []struct {
			cat memory.Category
			key string
		}{{memory.CategoryPreference, "occupation"}, {memory.CategoryProfileFact, "employer"}} {
			slot, err = r.GetActiveSlotForUpdate(ctx, u, miss.cat, miss.key)
			require.NoError(t, err)
			require.Nil(t, slot, "another category or key is another slot")
		}
	})
	require.Empty(t, h.ids(t, h.NewUser(t)))
}

func memSlotUnique(t *testing.T, h MemoryHarness) {
	ctx := context.Background()
	u, other := h.NewUser(t), h.NewUser(t)
	h.put(t, mkMem(u, 1, memory.CategoryProfileFact, "occupation"))

	err := h.Store.WithinTx(ctx, func(ctx context.Context, tx memory.Tx) error {
		return tx.Memories().Insert(ctx, u, mkMem(u, 2, memory.CategoryProfileFact, "occupation"))
	})
	require.Error(t, err, "a second ACTIVE row of the slot violates ux_memories_active_slot")
	require.Equal(t, []uuid.UUID{mid(1)}, h.ids(t, u), "the refused insert left nothing behind")

	// another user, another category and SUPERSEDED rows share the slot name freely
	h.put(t,
		mkMem(other, 3, memory.CategoryProfileFact, "occupation"),
		mkMem(u, 4, memory.CategoryPreference, "occupation"),
		mkMem(u, 5, memory.CategoryProfileFact, "occupation", func(m *memory.Memory) { m.Status = memory.StatusSuperseded }),
		mkMem(u, 6, memory.CategoryProfileFact, "occupation", func(m *memory.Memory) { m.Status = memory.StatusSuperseded }),
	)
	require.Len(t, h.Dump(t, u), 4)

	// one tombstone per slot
	tomb := func(n int) memory.Memory {
		return mkMem(u, n, memory.CategoryPreference, "hobby", func(m *memory.Memory) {
			m.Status, m.EvidenceType, m.Content, m.DisplayText, m.Embedding, m.EmbeddingModel = memory.StatusDeleted, memory.EvidenceUserDeleted, "", "", nil, ""
			m.Confidence, m.Importance = 0, 0
		})
	}
	h.put(t, tomb(7))
	err = h.Store.WithinTx(ctx, func(ctx context.Context, tx memory.Tx) error { return tx.Memories().Insert(ctx, u, tomb(8)) })
	require.Error(t, err, "a second tombstone of the slot violates ux_memories_tombstone_slot")

	// an insert for a row that carries another user is refused
	err = h.Store.WithinTx(ctx, func(ctx context.Context, tx memory.Tx) error {
		return tx.Memories().Insert(ctx, u, mkMem(other, 9, memory.CategoryProfileFact, "x"))
	})
	require.Error(t, err)
	require.False(t, containsID(h.ids(t, other), mid(9)))
}

func containsID(ids []uuid.UUID, id uuid.UUID) bool {
	for _, x := range ids {
		if x == id {
			return true
		}
	}
	return false
}

func memReinforce(t *testing.T, h MemoryHarness) {
	u := h.NewUser(t)
	sess, pv := h.NewSession(t, u), h.PromptVersionID
	for i, c := range []struct{ old, cand, want float64 }{
		{0.90, 0.95, 0.99}, // min(0.990, max(0.90, 0.95) + 0.050)
		{0.70, 0.60, 0.75}, // the old confidence is the maximum
		{0.80, 0.90, 0.95},
		{0.94, 0.60, 0.99}, // exactly at the ceiling
		{0.985, 0.70, 0.99},
	} {
		require.InDelta(t, c.want, memory.ReinforcedConfidence(c.old, c.cand), 1e-9, "case %d: the shared formula", i)
		n := 10 + i
		old := mkMem(u, n, memory.CategoryPreference, "k"+string(rune('a'+i)), func(m *memory.Memory) {
			m.Confidence, m.Importance, m.ObservationCount = c.old, 0.5, 2
			m.ExpiresAt = memAt.Add(100 * 24 * time.Hour)
		})
		h.put(t, old)
		later := memAt.Add(48 * time.Hour)
		turns := []uuid.UUID{{0: 1}, {0: 2}}
		h.tx(t, func(ctx context.Context, r memory.Repository) {
			require.NoError(t, r.Reinforce(ctx, u, old.ID, memory.ReinforceInput{
				Confidence: c.cand, Importance: 0.7, ExpiresAt: later.Add(365 * 24 * time.Hour),
				SourceSessionID: sess, SourceTurnIDs: turns, PromptVersionID: &pv, Now: later,
			}))
		})
		got := h.mustRow(t, u, old.ID)
		require.InDelta(t, c.want, got.Confidence, 1e-9, "case %d", i)
		require.Equal(t, 3, got.ObservationCount)
		require.InDelta(t, 0.7, got.Importance, 1e-9, "importance is the larger one")
		require.True(t, later.Equal(got.LastObservedAt))
		require.True(t, later.Add(365*24*time.Hour).Equal(got.ExpiresAt), "expiry grows to the new TTL")
		require.Equal(t, &sess, got.SourceSessionID)
		require.Equal(t, turns, got.SourceTurnIDs)
		require.Equal(t, &pv, got.ExtractionPromptVersionID)
		require.Equal(t, "内容", got.Content, "content is never rewritten")
		require.True(t, memAt.Equal(got.FirstObservedAt))
		h.tx(t, func(ctx context.Context, r memory.Repository) {
			slot, err := r.GetActiveSlotForUpdate(ctx, u, memory.CategoryPreference, old.Key)
			require.NoError(t, err)
			require.Equal(t, vec(1, 0), slot.Embedding, "the embedding is never replaced")
		})
	}

	// a lower importance and an earlier expiry never shrink the row; no prompt version keeps the old one
	keep := mkMem(u, 30, memory.CategoryPreference, "keep", func(m *memory.Memory) {
		m.Importance, m.ExpiresAt, m.ExtractionPromptVersionID = 0.8, memAt.Add(500*24*time.Hour), &pv
	})
	h.put(t, keep)
	h.tx(t, func(ctx context.Context, r memory.Repository) {
		require.NoError(t, r.Reinforce(ctx, u, keep.ID, memory.ReinforceInput{
			Confidence: 0.8, Importance: 0.2, ExpiresAt: memAt.Add(10 * 24 * time.Hour), SourceSessionID: sess, Now: memAt.Add(time.Hour),
		}))
	})
	got := h.mustRow(t, u, keep.ID)
	require.InDelta(t, 0.8, got.Importance, 1e-9)
	require.True(t, keep.ExpiresAt.Equal(got.ExpiresAt))
	require.Equal(t, &pv, got.ExtractionPromptVersionID)
}

func memReinforceNotFound(t *testing.T, h MemoryHarness) {
	ctx := context.Background()
	u := h.NewUser(t)
	sess := h.NewSession(t, u)
	h.put(t, mkMem(u, 1, memory.CategoryPreference, "old", func(m *memory.Memory) { m.Status = memory.StatusSuperseded }))
	for _, id := range []uuid.UUID{mid(1), mid(99)} {
		err := h.Store.WithinTx(ctx, func(ctx context.Context, tx memory.Tx) error {
			return tx.Memories().Reinforce(ctx, u, id, memory.ReinforceInput{Confidence: 0.9, SourceSessionID: sess, ExpiresAt: memAt, Now: memAt})
		})
		require.ErrorIs(t, err, memory.ErrNotFound)
	}
	require.Equal(t, 1, h.mustRow(t, u, mid(1)).ObservationCount)
}

func memSupersede(t *testing.T, h MemoryHarness) {
	u := h.NewUser(t)
	old := mkMem(u, 1, memory.CategoryProfileFact, "occupation")
	h.put(t, old)
	exp := memAt.Add(30 * 24 * time.Hour)
	h.tx(t, func(ctx context.Context, r memory.Repository) {
		// the UPDATE comes first so the ACTIVE slot is free for the INSERT
		require.NoError(t, r.Supersede(ctx, u, old.ID, mid(2), exp, memAt))
		require.NoError(t, r.Insert(ctx, u, mkMem(u, 2, memory.CategoryProfileFact, "occupation", func(m *memory.Memory) { m.Content = "新" })))
	})
	o := h.mustRow(t, u, mid(1))
	require.Equal(t, memory.StatusSuperseded, o.Status)
	by := mid(2)
	require.Equal(t, &by, o.SupersededBy)
	require.True(t, exp.Equal(o.ExpiresAt))
	n := h.mustRow(t, u, mid(2))
	require.Equal(t, memory.StatusActive, n.Status)
	require.Nil(t, n.SupersededBy)
	h.tx(t, func(ctx context.Context, r memory.Repository) {
		slot, err := r.GetActiveSlotForUpdate(ctx, u, memory.CategoryProfileFact, "occupation")
		require.NoError(t, err)
		require.Equal(t, mid(2), slot.ID, "the new row owns the slot")
	})

	// the retired row cannot be superseded again, nor a missing one
	for _, id := range []uuid.UUID{mid(1), mid(99)} {
		err := h.Store.WithinTx(context.Background(), func(ctx context.Context, tx memory.Tx) error {
			return tx.Memories().Supersede(ctx, u, id, mid(3), exp, memAt)
		})
		require.ErrorIs(t, err, memory.ErrNotFound)
	}
}

func memDeleteChain(t *testing.T, h MemoryHarness) {
	u := h.NewUser(t)
	sup := func(by int) func(*memory.Memory) {
		return func(m *memory.Memory) { b := mid(by); m.Status, m.SupersededBy = memory.StatusSuperseded, &b }
	}
	h.put(t,
		mkMem(u, 3, memory.CategoryProfileFact, "occupation"),
		mkMem(u, 2, memory.CategoryProfileFact, "occupation", sup(3)),
		mkMem(u, 1, memory.CategoryProfileFact, "occupation", sup(2)),
		mkMem(u, 4, memory.CategoryProfileFact, "employer"), // another slot stays
	)
	var ok bool
	h.tx(t, func(ctx context.Context, r memory.Repository) {
		var err error
		ok, err = r.Delete(ctx, u, mid(3), memAt)
		require.NoError(t, err)
	})
	require.True(t, ok)
	require.False(t, containsID(h.ids(t, u), mid(1)) || containsID(h.ids(t, u), mid(2)) || containsID(h.ids(t, u), mid(3)), "the row and its whole supersede chain are gone")
	require.True(t, containsID(h.ids(t, u), mid(4)))

	var tomb []memory.Memory
	for _, m := range h.Dump(t, u) {
		if m.Status == memory.StatusDeleted {
			tomb = append(tomb, m)
		}
	}
	require.Len(t, tomb, 1)
	require.Equal(t, memory.CategoryProfileFact, tomb[0].Category)
	require.Equal(t, "occupation", tomb[0].Key)
	require.Empty(t, tomb[0].Content)
	require.Empty(t, tomb[0].DisplayText)
	require.Equal(t, memory.EvidenceUserDeleted, tomb[0].EvidenceType)
	require.Zero(t, tomb[0].Confidence)
	require.Zero(t, tomb[0].Importance)
	require.True(t, memAt.Add(90*24*time.Hour).Equal(tomb[0].ExpiresAt), "tombstone lives 90 days")
	require.NotEqual(t, mid(3), tomb[0].ID)
}

func memDeleteUpsert(t *testing.T, h MemoryHarness) {
	u := h.NewUser(t)
	h.put(t, mkMem(u, 1, memory.CategoryPreference, "hobby"))
	del := func(id uuid.UUID, now time.Time) bool {
		var ok bool
		h.tx(t, func(ctx context.Context, r memory.Repository) {
			var err error
			ok, err = r.Delete(ctx, u, id, now)
			require.NoError(t, err)
		})
		return ok
	}
	require.True(t, del(mid(1), memAt))
	// the slot is reused, then deleted again later: still one tombstone, its life restarts
	h.put(t, mkMem(u, 2, memory.CategoryPreference, "hobby"))
	later := memAt.Add(40 * 24 * time.Hour)
	require.True(t, del(mid(2), later))
	tombs := h.Dump(t, u)
	require.Len(t, tombs, 1, "ON CONFLICT upserts the tombstone instead of adding one")
	require.Equal(t, memory.StatusDeleted, tombs[0].Status)
	require.True(t, later.Add(90*24*time.Hour).Equal(tombs[0].ExpiresAt))
}

func memDeleteEpisodic(t *testing.T, h MemoryHarness) {
	u := h.NewUser(t)
	h.put(t, mkMem(u, 1, memory.CategoryEpisodic, "episode:"+mid(1).String()))
	h.tx(t, func(ctx context.Context, r memory.Repository) {
		ok, err := r.Delete(ctx, u, mid(1), memAt)
		require.NoError(t, err)
		require.True(t, ok)
	})
	require.Empty(t, h.Dump(t, u), "an episode has no slot to protect: no tombstone")
}

func memDeleteMissing(t *testing.T, h MemoryHarness) {
	u := h.NewUser(t)
	h.put(t, mkMem(u, 1, memory.CategoryPreference, "old", func(m *memory.Memory) { m.Status = memory.StatusSuperseded }))
	h.tx(t, func(ctx context.Context, r memory.Repository) {
		for _, id := range []uuid.UUID{mid(1), mid(99)} {
			ok, err := r.Delete(ctx, u, id, memAt)
			require.NoError(t, err)
			require.False(t, ok, "missing and non-ACTIVE look the same")
		}
	})
	require.Equal(t, []uuid.UUID{mid(1)}, h.ids(t, u), "no row removed and no tombstone made")
}

func memTombstoneExpiry(t *testing.T, h MemoryHarness) {
	u := h.NewUser(t)
	h.put(t, mkMem(u, 1, memory.CategoryPreference, "hobby"), mkMem(u, 2, memory.CategoryProfileFact, "hobby"))
	h.tx(t, func(ctx context.Context, r memory.Repository) {
		ok, err := r.Delete(ctx, u, mid(1), memAt)
		require.NoError(t, err)
		require.True(t, ok)
	})
	expires := memAt.Add(90 * 24 * time.Hour)
	h.tx(t, func(ctx context.Context, r memory.Repository) {
		for _, c := range []struct {
			cat  memory.Category
			key  string
			now  time.Time
			want bool
		}{
			{memory.CategoryPreference, "hobby", memAt, true},
			{memory.CategoryPreference, "hobby", expires.Add(-time.Second), true},
			{memory.CategoryPreference, "hobby", expires, false}, // expires_at > now
			{memory.CategoryPreference, "hobby", expires.Add(time.Second), false},
			{memory.CategoryProfileFact, "hobby", memAt, false}, // another category
			{memory.CategoryPreference, "pet", memAt, false},    // another key
		} {
			got, err := r.TombstoneActive(ctx, u, c.cat, c.key, c.now)
			require.NoError(t, err)
			require.Equal(t, c.want, got, "%s/%s at %s", c.cat, c.key, c.now)
		}
	})
}

func memCountActive(t *testing.T, h MemoryHarness) {
	u := h.NewUser(t)
	h.put(t,
		mkMem(u, 1, memory.CategoryProfileFact, "a"), mkMem(u, 2, memory.CategoryPreference, "b"),
		mkMem(u, 3, memory.CategoryPreference, "c", func(m *memory.Memory) { m.Status = memory.StatusSuperseded }),
		mkMem(u, 4, memory.CategoryLearningPattern, "d"),
		mkMem(u, 5, memory.CategoryEpisodic, "episode:5"), mkMem(u, 6, memory.CategoryEpisodic, "episode:6"),
		mkMem(u, 7, memory.CategoryEpisodic, "episode:7", func(m *memory.Memory) { m.Status = memory.StatusSuperseded }),
	)
	h.tx(t, func(ctx context.Context, r memory.Repository) {
		for g, want := range map[memory.CapGroup]int{memory.CapFactPref: 2, memory.CapLearning: 1, memory.CapEpisodic: 2} {
			n, err := r.CountActive(ctx, u, g)
			require.NoError(t, err)
			require.Equal(t, want, n, string(g))
		}
	})
}

func memDeleteLowestScore(t *testing.T, h MemoryHarness) {
	u := h.NewUser(t)
	score := func(imp, conf float64, last time.Duration) func(*memory.Memory) {
		return func(m *memory.Memory) { m.Importance, m.Confidence, m.LastObservedAt = imp, conf, memAt.Add(last) }
	}
	h.put(t,
		mkMem(u, 1, memory.CategoryProfileFact, "a", score(0.9, 0.9, 0)),              // 0.90
		mkMem(u, 2, memory.CategoryProfileFact, "b", score(0.2, 0.9, 0)),              // 0.48: lowest
		mkMem(u, 5, memory.CategoryPreference, "e", score(0.5, 0.5, 0)),               // 0.50 as 3 but inserted first: physical order must not decide
		mkMem(u, 4, memory.CategoryPreference, "d", score(0.5, 0.5, -time.Hour)),      // 0.50, older: goes first
		mkMem(u, 3, memory.CategoryPreference, "c", score(0.5, 0.5, 0)),               // 0.50
		mkMem(u, 6, memory.CategoryLearningPattern, "f", score(0, 0, -24*time.Hour)),  // another group
		mkMem(u, 7, memory.CategoryPreference, "g", score(0, 0, 0)),                   // SUPERSEDED rows never count
		mkMem(u, 8, memory.CategoryEpisodic, "episode:8", score(0, 0, -48*time.Hour)), // another group
	)
	h.tx(t, func(ctx context.Context, r memory.Repository) {
		// make row 7 non-ACTIVE after the fact through the public API
		require.NoError(t, r.Supersede(ctx, u, mid(7), mid(9), memAt.Add(time.Hour), memAt))
		require.NoError(t, r.Insert(ctx, u, mkMem(u, 9, memory.CategoryPreference, "g", score(1, 1, 0))))
	})
	h.tx(t, func(ctx context.Context, r memory.Repository) {
		require.NoError(t, r.DeleteLowest(ctx, u, memory.CapFactPref, 2))
	})
	got := h.ids(t, u)
	require.False(t, containsID(got, mid(2)), "0.6*importance + 0.4*confidence = 0.48 is the lowest")
	require.False(t, containsID(got, mid(4)), "equal score: the oldest observation goes first")
	for _, keep := range []int{1, 3, 5, 6, 7, 8, 9} {
		require.True(t, containsID(got, mid(keep)), "row %d stays", keep)
	}
	h.tx(t, func(ctx context.Context, r memory.Repository) {
		require.NoError(t, r.DeleteLowest(ctx, u, memory.CapFactPref, 1))
	})
	require.False(t, containsID(h.ids(t, u), mid(3)), "equal score and age: the lower id goes first")
	require.True(t, containsID(h.ids(t, u), mid(5)))

	// LEARNING_PATTERN is evicted by score too, not by age: the old, strong pattern outlives the new, weak one
	l := h.NewUser(t)
	h.put(t,
		mkMem(l, 41, memory.CategoryLearningPattern, "old_strong", score(0.9, 0.9, -48*time.Hour)),
		mkMem(l, 42, memory.CategoryLearningPattern, "new_weak", score(0.1, 0.1, 0)),
	)
	h.tx(t, func(ctx context.Context, r memory.Repository) {
		require.NoError(t, r.DeleteLowest(ctx, l, memory.CapLearning, 1))
	})
	require.Equal(t, []uuid.UUID{mid(41)}, h.ids(t, l))
}

func memDeleteLowestEpisodic(t *testing.T, h MemoryHarness) {
	u := h.NewUser(t)
	ep := func(n int, last time.Duration, imp, conf float64) memory.Memory {
		return mkMem(u, n, memory.CategoryEpisodic, "episode:"+mid(n).String(), func(m *memory.Memory) {
			m.LastObservedAt, m.Importance, m.Confidence = memAt.Add(last), imp, conf
		})
	}
	h.put(t,
		ep(1, -time.Hour, 1, 1), // oldest, best score: still goes first
		ep(3, 0, 0.1, 0.1),
		ep(2, 0, 0, 0),
		ep(4, time.Hour, 0, 0),
	)
	h.tx(t, func(ctx context.Context, r memory.Repository) {
		require.NoError(t, r.DeleteLowest(ctx, u, memory.CapEpisodic, 2))
	})
	require.ElementsMatch(t, []uuid.UUID{mid(3), mid(4)}, h.ids(t, u), "oldest observation first, then the lower id; score is ignored")
}

// memDeleteLowestTie evicts among rows that tie on score and age, inserted in descending id order, so
// neither the insertion order nor the key order (the slot index PostgreSQL scans) can stand in for
// the id tie-break: higher ids get the smaller keys.
func memDeleteLowestTie(t *testing.T, h MemoryHarness) {
	for i, g := range []struct {
		group memory.CapGroup
		cat   memory.Category
	}{{memory.CapFactPref, memory.CategoryPreference}, {memory.CapLearning, memory.CategoryLearningPattern}, {memory.CapEpisodic, memory.CategoryEpisodic}} {
		u := h.NewUser(t)
		off := 100 * (i + 1) // ids are unique across users in PostgreSQL
		var rows []memory.Memory
		for n := 26; n >= 21; n-- {
			rows = append(rows, mkMem(u, off+n, g.cat, "k"+string(rune('a'+26-n))))
		}
		h.put(t, rows...)
		h.tx(t, func(ctx context.Context, r memory.Repository) {
			require.NoError(t, r.DeleteLowest(ctx, u, g.group, 3))
		})
		require.ElementsMatch(t, []uuid.UUID{mid(off + 24), mid(off + 25), mid(off + 26)}, h.ids(t, u), "%s: the three lowest ids go first", g.group)
	}
}

func memDeleteLowestEdges(t *testing.T, h MemoryHarness) {
	u := h.NewUser(t)
	h.put(t, mkMem(u, 1, memory.CategoryLearningPattern, "a"), mkMem(u, 2, memory.CategoryLearningPattern, "b"), mkMem(u, 3, memory.CategoryPreference, "c"))
	h.tx(t, func(ctx context.Context, r memory.Repository) {
		require.NoError(t, r.DeleteLowest(ctx, u, memory.CapLearning, 0))
		require.NoError(t, r.DeleteLowest(ctx, u, memory.CapEpisodic, 5)) // empty group
	})
	require.Len(t, h.Dump(t, u), 3, "n=0 and an empty group delete nothing")
	h.tx(t, func(ctx context.Context, r memory.Repository) {
		require.NoError(t, r.DeleteLowest(ctx, u, memory.CapLearning, 10))
	})
	require.Equal(t, []uuid.UUID{mid(3)}, h.ids(t, u), "n larger than the group empties only that group")
}

func memListCore(t *testing.T, h MemoryHarness) {
	u := h.NewUser(t)
	obs := func(d time.Duration) func(*memory.Memory) {
		return func(m *memory.Memory) { m.LastObservedAt = memAt.Add(d) }
	}
	h.put(t,
		mkMem(u, 1, memory.CategoryPreference, "a", obs(-time.Hour)),
		mkMem(u, 2, memory.CategoryProfileFact, "b", obs(-2*time.Hour), func(m *memory.Memory) { m.Confidence, m.Importance, m.ObservationCount = 0.8, 0.6, 4 }),
		mkMem(u, 3, memory.CategoryLearningPattern, "c", obs(0)),
		mkMem(u, 4, memory.CategoryProfileFact, "d", obs(0)), // same time as 3: id order
		mkMem(u, 5, memory.CategoryProfileFact, "expired", func(m *memory.Memory) { m.ExpiresAt = memAt }),
		mkMem(u, 6, memory.CategoryProfileFact, "old", func(m *memory.Memory) { m.Status = memory.StatusSuperseded }),
		mkMem(u, 7, memory.CategoryEpisodic, "episode:7"),
		mkMem(u, 8, memory.CategoryProfileFact, "almost", func(m *memory.Memory) { m.ExpiresAt = memAt.Add(time.Second) }),
	)
	h.put(t, mkMem(u, 9, memory.CategoryPreference, "tomb", func(m *memory.Memory) {
		m.Status, m.EvidenceType, m.Content, m.DisplayText, m.Embedding, m.EmbeddingModel = memory.StatusDeleted, memory.EvidenceUserDeleted, "", "", nil, ""
		m.Confidence, m.Importance = 0, 0
	}))
	rows, err := h.Store.Reader().ListCore(context.Background(), u, memAt)
	require.NoError(t, err)
	require.Equal(t, []uuid.UUID{mid(3), mid(4), mid(8), mid(1), mid(2)},
		idsOf(rows, func(r memory.CoreRow) uuid.UUID { return r.ID }),
		"ACTIVE, unexpired, not episodic; newest observation first, then id")
	b := rows[4]
	require.Equal(t, memory.CategoryProfileFact, b.Category)
	require.Equal(t, "b", b.Key)
	require.Equal(t, "内容", b.Content)
	require.InDelta(t, 0.8, b.Confidence, 1e-9)
	require.InDelta(t, 0.6, b.Importance, 1e-9)
	require.Equal(t, 4, b.ObservationCount)
	require.True(t, memAt.Add(-2*time.Hour).Equal(b.LastObservedAt))
	require.Equal(t, vec(1, 0), b.Embedding)
}

func memSearchEpisodic(t *testing.T, h MemoryHarness) {
	u := h.NewUser(t)
	s1, s2 := h.NewSession(t, u), h.NewSession(t, u)
	ep := func(n int, v []float32, mod ...func(*memory.Memory)) memory.Memory {
		return mkMem(u, n, memory.CategoryEpisodic, "episode:"+mid(n).String(), append([]func(*memory.Memory){func(m *memory.Memory) { m.Embedding = v }}, mod...)...)
	}
	h.put(t,
		ep(1, vec(4, 3)), // cosine 0.8 with (1,0)
		ep(2, vec(1, 0), func(m *memory.Memory) { m.SourceSessionID = &s1 }),
		ep(3, vec(3, 4)), // 0.6: below the threshold
		ep(4, vec(1, 0), func(m *memory.Memory) { m.SourceSessionID = &s2 }),
		ep(5, vec(1, 0), func(m *memory.Memory) { m.ExpiresAt = memAt }),                // expired
		ep(6, vec(1, 0), func(m *memory.Memory) { m.Status = memory.StatusSuperseded }), // not ACTIVE
		ep(7, nil), // no embedding
		mkMem(u, 8, memory.CategoryPreference, "pref", func(m *memory.Memory) { m.Embedding = vec(1, 0) }), // not an episode
		ep(9, vec(1, 0)), // no source session (it was deleted): never excluded
		ep(10, vec(4, 3), func(m *memory.Memory) { m.SourceSessionID = &s1 }),
	)
	rd := h.Store.Reader()
	search := func(minSim float64, k int, ex *uuid.UUID) []memory.EpisodicRow {
		rows, err := rd.SearchEpisodic(context.Background(), u, vec(1, 0), minSim, k, ex, memAt)
		require.NoError(t, err)
		return rows
	}
	id := func(r memory.EpisodicRow) uuid.UUID { return r.ID }

	rows := search(0.7, 10, nil)
	require.Equal(t, []uuid.UUID{mid(2), mid(4), mid(9), mid(1), mid(10)}, idsOf(rows, id), "nearest first (ties by id); 0.6, expired, superseded, no embedding, other category out")
	require.InDelta(t, 1.0, rows[0].Similarity, 1e-6)
	require.InDelta(t, 0.8, rows[3].Similarity, 1e-6)
	require.Equal(t, "内容", rows[0].Content)
	require.True(t, memAt.Equal(rows[0].LastObservedAt))

	require.Equal(t, []uuid.UUID{mid(4), mid(9), mid(1)}, idsOf(search(0.7, 3, &s1), id), "the session being run is excluded; a NULL source session never is; k=3")
	require.Equal(t, []uuid.UUID{mid(2), mid(9), mid(1), mid(10)}, idsOf(search(0.7, 10, &s2), id))
	require.Len(t, search(0.7, 1, nil), 1)
	require.Equal(t, []uuid.UUID{mid(2), mid(4), mid(9)}, idsOf(search(1.0, 10, nil), id), "similarity equal to the threshold qualifies")
	require.Len(t, search(0.5, 10, nil), 6, "0.6 passes a lower threshold")
	require.Empty(t, search(1.01, 10, nil))
	require.Empty(t, search(0.7, 0, nil))
}

func memFindNearest(t *testing.T, h MemoryHarness) {
	u := h.NewUser(t)
	h.put(t,
		mkMem(u, 1, memory.CategoryPreference, "far", func(m *memory.Memory) { m.Embedding = vec(3, 4) }),
		mkMem(u, 2, memory.CategoryPreference, "near", func(m *memory.Memory) { m.Embedding = vec(4, 3) }),
		mkMem(u, 3, memory.CategoryProfileFact, "other_category", func(m *memory.Memory) { m.Embedding = vec(1, 0) }),
		mkMem(u, 4, memory.CategoryPreference, "retired", func(m *memory.Memory) { m.Embedding = vec(1, 0); m.Status = memory.StatusSuperseded }),
		mkMem(u, 5, memory.CategoryPreference, "no_vector", func(m *memory.Memory) { m.Embedding = nil }),
		mkMem(u, 6, memory.CategoryLearningPattern, "tie_b", func(m *memory.Memory) { m.Embedding = vec(1, 0) }),
		mkMem(u, 7, memory.CategoryLearningPattern, "tie_a", func(m *memory.Memory) { m.Embedding = vec(1, 0) }),
	)
	h.put(t, mkMem(u, 8, memory.CategoryPreference, "tomb", func(m *memory.Memory) {
		m.Status, m.EvidenceType, m.Content, m.DisplayText, m.Embedding, m.EmbeddingModel = memory.StatusDeleted, memory.EvidenceUserDeleted, "", "", nil, ""
		m.Confidence, m.Importance = 0, 0
	}))
	h.tx(t, func(ctx context.Context, r memory.Repository) {
		n, err := r.FindNearestActive(ctx, u, memory.CategoryPreference, vec(1, 0))
		require.NoError(t, err)
		require.NotNil(t, n)
		require.Equal(t, mid(2), n.ID, "nearest ACTIVE row of the category with a vector")
		require.Equal(t, "near", n.Key)
		require.InDelta(t, 0.8, n.Similarity, 1e-6, "similarity = 1 - cosine distance")

		n, err = r.FindNearestActive(ctx, u, memory.CategoryLearningPattern, vec(1, 0))
		require.NoError(t, err)
		require.Equal(t, mid(6), n.ID, "a tie goes to the lower id")
		require.InDelta(t, 1.0, n.Similarity, 1e-6)

		n, err = r.FindNearestActive(ctx, u, memory.CategoryEpisodic, vec(1, 0))
		require.NoError(t, err)
		require.Nil(t, n, "no ACTIVE row in the category")
	})
}

func memList(t *testing.T, h MemoryHarness) {
	u := h.NewUser(t)
	sess := h.NewSession(t, u)
	obs := func(d time.Duration) func(*memory.Memory) {
		return func(m *memory.Memory) { m.LastObservedAt = memAt.Add(d) }
	}
	h.put(t,
		mkMem(u, 1, memory.CategoryPreference, "a", obs(-time.Hour)),
		mkMem(u, 2, memory.CategoryProfileFact, "b", obs(-2*time.Hour), func(m *memory.Memory) {
			m.SourceSessionID, m.Content, m.DisplayText, m.ObservationCount, m.Confidence = &sess, "エンジニア", "Insinyur", 5, 0.95
		}),
		mkMem(u, 3, memory.CategoryProfileFact, "c", obs(0)),
		mkMem(u, 4, memory.CategoryProfileFact, "d", obs(0)),
		mkMem(u, 5, memory.CategoryEpisodic, "episode:5", obs(-time.Minute)),
		mkMem(u, 6, memory.CategoryEpisodic, "episode:6", obs(-time.Minute)),
		mkMem(u, 7, memory.CategoryLearningPattern, "e", obs(0)),
		mkMem(u, 8, memory.CategoryProfileFact, "expired", func(m *memory.Memory) { m.ExpiresAt = memAt }),
		mkMem(u, 9, memory.CategoryProfileFact, "old", func(m *memory.Memory) { m.Status = memory.StatusSuperseded }),
	)
	h.put(t, mkMem(u, 10, memory.CategoryPreference, "tomb", func(m *memory.Memory) {
		m.Status, m.EvidenceType, m.Content, m.DisplayText, m.Embedding, m.EmbeddingModel = memory.StatusDeleted, memory.EvidenceUserDeleted, "", "", nil, ""
		m.Confidence, m.Importance = 0, 0
	}))
	rd := h.Store.Reader()
	list := func(cat *memory.Category, after *memory.ListCursor, limit int) []memory.Memory {
		rows, err := rd.List(context.Background(), u, cat, after, limit, memAt)
		require.NoError(t, err)
		return rows
	}
	id := func(m memory.Memory) uuid.UUID { return m.ID }

	all := list(nil, nil, 100)
	want := []uuid.UUID{mid(5), mid(6), mid(7), mid(1), mid(3), mid(4), mid(2)}
	require.Equal(t, want, idsOf(all, id), "category, newest observation first, id; ACTIVE and unexpired only, no tombstone")
	b := all[6]
	require.Equal(t, "b", b.Key)
	require.Equal(t, "エンジニア", b.Content)
	require.Equal(t, "Insinyur", b.DisplayText)
	require.Equal(t, 5, b.ObservationCount)
	require.InDelta(t, 0.95, b.Confidence, 1e-9)
	require.Equal(t, &sess, b.SourceSessionID)
	require.Equal(t, memory.StatusActive, b.Status)
	require.Equal(t, u, b.UserID)
	require.Nil(t, b.Embedding, "List never loads the embedding")

	for _, limit := range []int{1, 2, 3, 7} { // keyset paging without gaps or repeats
		var seen []uuid.UUID
		var cur *memory.ListCursor
		for pages := 0; ; pages++ {
			require.Less(t, pages, 20, "limit %d: paging must terminate (the cursor is strictly after its row)", limit)
			page := list(nil, cur, limit)
			seen = append(seen, idsOf(page, id)...)
			if len(page) < limit {
				break
			}
			last := page[len(page)-1]
			cur = &memory.ListCursor{Category: last.Category, LastObservedAt: last.LastObservedAt, ID: last.ID}
		}
		require.Equal(t, want, seen, "limit %d", limit)
	}

	cat := memory.CategoryProfileFact
	require.Equal(t, []uuid.UUID{mid(3), mid(4), mid(2)}, idsOf(list(&cat, nil, 100), id))
	last := all[3] // PREFERENCE 1: the next page starts after it
	require.Equal(t, []uuid.UUID{mid(3), mid(4)}, idsOf(list(nil, &memory.ListCursor{Category: last.Category, LastObservedAt: last.LastObservedAt, ID: last.ID}, 2), id))
	require.Empty(t, list(nil, nil, 0))
}

func memDeleteAll(t *testing.T, h MemoryHarness) {
	a, b := h.NewUser(t), h.NewUser(t)
	for i, u := range []uuid.UUID{a, b} {
		base := i * 10
		h.put(t,
			mkMem(u, base+1, memory.CategoryPreference, "x"),
			mkMem(u, base+2, memory.CategoryPreference, "old", func(m *memory.Memory) { m.Status = memory.StatusSuperseded }),
			mkMem(u, base+3, memory.CategoryProfileFact, "y"),
		)
		h.tx(t, func(ctx context.Context, r memory.Repository) {
			ok, err := r.Delete(ctx, u, mid(base+3), memAt)
			require.NoError(t, err)
			require.True(t, ok) // leaves a tombstone
		})
	}
	h.tx(t, func(ctx context.Context, r memory.Repository) { require.NoError(t, r.DeleteAll(ctx, a)) })
	require.Empty(t, h.Dump(t, a), "active, superseded and tombstone rows go, and no tombstone is made")
	require.Len(t, h.Dump(t, b), 3, "the other user keeps everything, tombstone included")
	h.tx(t, func(ctx context.Context, r memory.Repository) { require.NoError(t, r.DeleteAll(ctx, a)) }) // idempotent
}

// seedTwins gives a and b the same rows (same slots, identical embeddings); b's ids are the lower
// ones, so a query that forgot the user predicate would surface b's rows first.
func seedTwins(t *testing.T, h MemoryHarness, a, b uuid.UUID, sessA, sessB uuid.UUID) {
	for _, c := range []struct {
		u    uuid.UUID
		base int
		sess uuid.UUID
	}{{b, 10, sessB}, {a, 20, sessA}} {
		sess := c.sess
		h.put(t,
			mkMem(c.u, c.base+1, memory.CategoryProfileFact, "occupation", func(m *memory.Memory) { m.SourceSessionID = &sess }),
			mkMem(c.u, c.base+2, memory.CategoryPreference, "hobby"),
			mkMem(c.u, c.base+3, memory.CategoryLearningPattern, "weak"),
			mkMem(c.u, c.base+4, memory.CategoryEpisodic, "episode:"+mid(c.base+4).String(), func(m *memory.Memory) { m.SourceSessionID = &sess }),
		)
	}
}

func memIsolationReads(t *testing.T, h MemoryHarness) {
	ctx := context.Background()
	a, b, c := h.NewUser(t), h.NewUser(t), h.NewUser(t)
	sa, sb := h.NewSession(t, a), h.NewSession(t, b)
	seedTwins(t, h, a, b, sa, sb)
	rd := h.Store.Reader()

	core, err := rd.ListCore(ctx, a, memAt)
	require.NoError(t, err)
	require.Equal(t, []uuid.UUID{mid(21), mid(22), mid(23)}, idsOf(core, func(r memory.CoreRow) uuid.UUID { return r.ID }))
	hits, err := rd.SearchEpisodic(ctx, a, vec(1, 0), 0.75, 3, nil, memAt)
	require.NoError(t, err)
	require.Equal(t, []uuid.UUID{mid(24)}, idsOf(hits, func(r memory.EpisodicRow) uuid.UUID { return r.ID }), "identical embeddings never leak")
	hits, err = rd.SearchEpisodic(ctx, a, vec(1, 0), 0.75, 3, &sa, memAt)
	require.NoError(t, err)
	require.Empty(t, hits, "excluding a's own session leaves nothing: b's identical episode is not a's")
	list, err := rd.List(ctx, a, nil, nil, 100, memAt)
	require.NoError(t, err)
	require.Equal(t, []uuid.UUID{mid(24), mid(23), mid(22), mid(21)}, idsOf(list, func(m memory.Memory) uuid.UUID { return m.ID }))

	// a user without memories sees nothing anywhere
	core, err = rd.ListCore(ctx, c, memAt)
	require.NoError(t, err)
	require.Empty(t, core)
	hits, err = rd.SearchEpisodic(ctx, c, vec(1, 0), 0.75, 3, nil, memAt)
	require.NoError(t, err)
	require.Empty(t, hits)
	list, err = rd.List(ctx, c, nil, nil, 100, memAt)
	require.NoError(t, err)
	require.Empty(t, list)

	h.tx(t, func(ctx context.Context, r memory.Repository) {
		for _, cat := range []memory.Category{memory.CategoryProfileFact, memory.CategoryPreference, memory.CategoryLearningPattern, memory.CategoryEpisodic} {
			n, err := r.FindNearestActive(ctx, a, cat, vec(1, 0))
			require.NoError(t, err)
			require.NotNil(t, n)
			require.Greater(t, n.ID.String(), mid(19).String(), "never one of b's (lower) ids: %s", cat)
			n, err = r.FindNearestActive(ctx, c, cat, vec(1, 0))
			require.NoError(t, err)
			require.Nil(t, n, "a user without memories has no nearest")
			slot, err := r.GetActiveSlotForUpdate(ctx, c, cat, map[memory.Category]string{
				memory.CategoryProfileFact: "occupation", memory.CategoryPreference: "hobby", memory.CategoryLearningPattern: "weak",
				memory.CategoryEpisodic: "episode:" + mid(24).String(),
			}[cat])
			require.NoError(t, err)
			require.Nil(t, slot)
			cnt, err := r.CountActive(ctx, c, memory.CapFactPref)
			require.NoError(t, err)
			require.Zero(t, cnt)
		}
		slot, err := r.GetActiveSlotForUpdate(ctx, a, memory.CategoryProfileFact, "occupation")
		require.NoError(t, err)
		require.Equal(t, mid(21), slot.ID, "the slot of a resolves to a's row though b has the same slot")
		cnt, err := r.CountActive(ctx, a, memory.CapFactPref)
		require.NoError(t, err)
		require.Equal(t, 2, cnt, "a's count, not a+b")
	})
}

func memIsolationWrites(t *testing.T, h MemoryHarness) {
	ctx := context.Background()
	a, b := h.NewUser(t), h.NewUser(t)
	sa, sb := h.NewSession(t, a), h.NewSession(t, b)
	seedTwins(t, h, a, b, sa, sb)
	before := h.Dump(t, b)

	err := h.Store.WithinTx(ctx, func(ctx context.Context, tx memory.Tx) error {
		return tx.Memories().Reinforce(ctx, a, mid(11), memory.ReinforceInput{Confidence: 0.99, Importance: 1, SourceSessionID: sa, ExpiresAt: memAt.Add(1000 * time.Hour), Now: memAt.Add(time.Hour)})
	})
	require.ErrorIs(t, err, memory.ErrNotFound, "a cannot reinforce b's row")
	err = h.Store.WithinTx(ctx, func(ctx context.Context, tx memory.Tx) error {
		return tx.Memories().Supersede(ctx, a, mid(11), mid(99), memAt.Add(time.Hour), memAt)
	})
	require.ErrorIs(t, err, memory.ErrNotFound, "a cannot supersede b's row")
	h.tx(t, func(ctx context.Context, r memory.Repository) {
		for _, id := range []uuid.UUID{mid(11), mid(12), mid(13), mid(14)} {
			ok, err := r.Delete(ctx, a, id, memAt)
			require.NoError(t, err)
			require.False(t, ok, "a cannot delete b's row %s", id)
		}
	})
	require.Equal(t, before, h.Dump(t, b), "b is untouched: no change, no tombstone")
	require.Len(t, h.Dump(t, a), 4)

	// a's DeleteAll takes none of b's
	h.tx(t, func(ctx context.Context, r memory.Repository) { require.NoError(t, r.DeleteAll(ctx, a)) })
	require.Empty(t, h.Dump(t, a))
	require.Equal(t, before, h.Dump(t, b))
}

func memIsolationCap(t *testing.T, h MemoryHarness) {
	a, b := h.NewUser(t), h.NewUser(t)
	// b's rows score lower and are older than a's: an unscoped eviction would take them
	for i := 1; i <= 3; i++ {
		h.put(t,
			mkMem(b, i, memory.CategoryPreference, "k"+string(rune('a'+i)), func(m *memory.Memory) {
				m.Importance, m.Confidence = 0.1, 0.1
				m.LastObservedAt = memAt.Add(-time.Hour)
			}),
			mkMem(b, 10+i, memory.CategoryEpisodic, "episode:"+mid(10+i).String(), func(m *memory.Memory) { m.LastObservedAt = memAt.Add(-time.Hour) }),
			mkMem(b, 20+i, memory.CategoryLearningPattern, "l"+string(rune('a'+i)), func(m *memory.Memory) { m.Importance, m.Confidence = 0.1, 0.1 }),
		)
	}
	h.put(t,
		mkMem(a, 31, memory.CategoryPreference, "x"),
		mkMem(a, 32, memory.CategoryPreference, "y", func(m *memory.Memory) { m.Importance, m.Confidence = 0.3, 0.3 }),
		mkMem(a, 33, memory.CategoryEpisodic, "episode:33"),
		mkMem(a, 34, memory.CategoryEpisodic, "episode:34", func(m *memory.Memory) { m.LastObservedAt = memAt.Add(-time.Minute) }),
		mkMem(a, 35, memory.CategoryLearningPattern, "z"),
	)
	h.tx(t, func(ctx context.Context, r memory.Repository) {
		require.NoError(t, r.DeleteLowest(ctx, a, memory.CapFactPref, 1))
		require.NoError(t, r.DeleteLowest(ctx, a, memory.CapEpisodic, 1))
		require.NoError(t, r.DeleteLowest(ctx, a, memory.CapLearning, 5))
	})
	require.ElementsMatch(t, []uuid.UUID{mid(31), mid(33)}, h.ids(t, a), "a lost its own lowest, oldest and whole group")
	require.Len(t, h.Dump(t, b), 9, "b lost nothing")

	h.tx(t, func(ctx context.Context, r memory.Repository) {
		require.NoError(t, r.DeleteLowest(ctx, b, memory.CapFactPref, 100))
	})
	require.Len(t, h.Dump(t, b), 6)
	require.ElementsMatch(t, []uuid.UUID{mid(31), mid(33)}, h.ids(t, a), "a is untouched by b's eviction")
}

func memIsolationTombstone(t *testing.T, h MemoryHarness) {
	a, b := h.NewUser(t), h.NewUser(t)
	h.put(t, mkMem(b, 1, memory.CategoryPreference, "hobby"), mkMem(a, 2, memory.CategoryPreference, "hobby"))
	h.tx(t, func(ctx context.Context, r memory.Repository) {
		ok, err := r.Delete(ctx, b, mid(1), memAt)
		require.NoError(t, err)
		require.True(t, ok)
	})
	h.tx(t, func(ctx context.Context, r memory.Repository) {
		got, err := r.TombstoneActive(ctx, b, memory.CategoryPreference, "hobby", memAt)
		require.NoError(t, err)
		require.True(t, got)
		got, err = r.TombstoneActive(ctx, a, memory.CategoryPreference, "hobby", memAt)
		require.NoError(t, err)
		require.False(t, got, "b's tombstone does not block a's slot")
	})
	require.Equal(t, []uuid.UUID{mid(2)}, h.ids(t, a), "a still holds the slot")

	// a's delete tombstones a's slot only; b's tombstone is neither renewed nor duplicated
	later := memAt.Add(10 * 24 * time.Hour)
	h.tx(t, func(ctx context.Context, r memory.Repository) {
		ok, err := r.Delete(ctx, a, mid(2), later)
		require.NoError(t, err)
		require.True(t, ok)
	})
	require.Len(t, h.Dump(t, a), 1)
	bt := h.Dump(t, b)
	require.Len(t, bt, 1)
	require.True(t, memAt.Add(90*24*time.Hour).Equal(bt[0].ExpiresAt), "b's tombstone keeps its own expiry")
}

func memRollback(t *testing.T, h MemoryHarness) {
	ctx := context.Background()
	u := h.NewUser(t)
	boom := errors.New("boom")
	err := h.Store.WithinTx(ctx, func(ctx context.Context, tx memory.Tx) error {
		if err := tx.Memories().Insert(ctx, u, mkMem(u, 1, memory.CategoryEpisodic, "episode:1")); err != nil {
			return err
		}
		return boom
	})
	require.ErrorIs(t, err, boom)
	require.Empty(t, h.Dump(t, u), "a failed transaction leaves nothing")

	h.put(t, mkMem(u, 2, memory.CategoryEpisodic, "episode:2"))
	hits, err := h.Store.Reader().SearchEpisodic(ctx, u, vec(1, 0), 0.75, 3, nil, memAt)
	require.NoError(t, err)
	require.Equal(t, []uuid.UUID{mid(2)}, idsOf(hits, func(r memory.EpisodicRow) uuid.UUID { return r.ID }), "a commit is visible to the reader")
	h.tx(t, func(ctx context.Context, r memory.Repository) { require.NoError(t, r.LockUser(ctx, u)) })
}
