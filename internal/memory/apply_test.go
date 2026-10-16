package memory_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/memory"
	"github.com/nabePi/kotoki-backend/internal/platform/clock"
	"github.com/nabePi/kotoki-backend/internal/platform/idgen"
	"github.com/nabePi/kotoki-backend/internal/testutil/fakes"
)

var t0 = time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC)

const dim = 100

// vec has ones in [from, to) of a 100-dimensional space: cosine(vec(0,100), vec(0,k)) = sqrt(k)/10,
// so k=81 is exactly 0.90, k=90 is 0.949, k=80 is 0.894, k=25 is 0.5 and disjoint ranges are 0.
func vec(from, to int) []float32 {
	v := make([]float32, dim)
	for i := from; i < to; i++ {
		v[i] = 1
	}
	return v
}

var base = vec(0, 100)

type fixture struct {
	t     *testing.T
	store *fakes.MemoryStore
	clk   *clock.Fake
	reg   *prometheus.Registry
	ap    *memory.Applier
	user  uuid.UUID
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	clk := clock.NewFake(t0)
	m, reg := newMetrics(t)
	return &fixture{
		t: t, store: fakes.NewMemoryStore(fakes.WithMemoryClock(clk)), clk: clk, reg: reg,
		ap: memory.NewApplier(idgen.NewSequence(t), clk, m), user: uuidFor(1),
	}
}

// seed stores an ACTIVE memory of f.user.
func (f *fixture) seed(cat memory.Category, key string, emb []float32, mod ...func(*memory.Memory)) memory.Memory {
	f.t.Helper()
	id := uuid.MustParse(fmt.Sprintf("01920000-0000-7000-8000-5eed%08x", len(f.store.All(f.user))+1))
	sess := uuidFor(900)
	m := memory.Memory{
		ID: id, UserID: f.user, Category: cat, Key: key, Content: "既存", DisplayText: "lama",
		EvidenceType: memory.EvidenceExplicit, Confidence: 0.90, Importance: 0.50, ObservationCount: 1,
		SourceSessionID: &sess, SourceTurnIDs: []uuid.UUID{uuidFor(800)}, Embedding: emb, Status: memory.StatusActive,
		FirstObservedAt: t0.Add(-48 * time.Hour), LastObservedAt: t0.Add(-24 * time.Hour), ExpiresAt: t0.Add(100 * 24 * time.Hour),
		CreatedAt: t0.Add(-48 * time.Hour), UpdatedAt: t0.Add(-24 * time.Hour),
	}
	for _, fn := range mod {
		fn(&m)
	}
	f.store.Put(m)
	return m
}

func cand(cat memory.Category, key string, emb []float32, mod ...func(*memory.Candidate)) memory.Candidate {
	c := memory.Candidate{
		Category: cat, Key: key, ContentJA: "新しい内容", DisplayID: "baru", EvidenceType: memory.EvidenceExplicit,
		Confidence: 0.95, Importance: 0.70, TurnIDs: []uuid.UUID{uuidFor(801), uuidFor(802)}, Embedding: emb,
		SessionID: uuidFor(901), EmbeddingModel: "emb-test",
	}
	for _, fn := range mod {
		fn(&c)
	}
	return c
}

func (f *fixture) apply(c memory.Candidate) memory.Action {
	f.t.Helper()
	return f.applyFor(f.user, c)
}

func (f *fixture) applyFor(user uuid.UUID, c memory.Candidate) memory.Action {
	f.t.Helper()
	var act memory.Action
	err := f.store.WithinTx(context.Background(), func(ctx context.Context, tx memory.Tx) error {
		var err error
		act, err = f.ap.Apply(ctx, tx, user, c)
		return err
	})
	require.NoError(f.t, err)
	return act
}

func (f *fixture) get(id uuid.UUID) memory.Memory {
	f.t.Helper()
	m, ok := f.store.Get(f.user, id)
	require.True(f.t, ok)
	return m
}

func (f *fixture) active(cat memory.Category) []memory.Memory {
	var out []memory.Memory
	for _, m := range f.store.All(f.user) {
		if m.Status == memory.StatusActive && m.Category == cat {
			out = append(out, m)
		}
	}
	return out
}

func days(n int) time.Duration { return time.Duration(n) * 24 * time.Hour }

func TestApply_SameSlotSim095_Reinforces(t *testing.T) {
	f := newFixture(t)
	old := f.seed(memory.CategoryProfileFact, "occupation", base)
	c := cand(memory.CategoryProfileFact, "occupation", vec(0, 90)) // cosine 0.949
	c.PromptVersionID = ptr(uuidFor(77))

	require.Equal(t, memory.ActionReinforced, f.apply(c))

	got := f.get(old.ID)
	require.Equal(t, 2, got.ObservationCount)
	require.InDelta(t, 0.99, got.Confidence, 1e-9, "min(0.99, max(0.90,0.95)+0.05)")
	require.InDelta(t, 0.70, got.Importance, 1e-9, "max(old, new)")
	require.Equal(t, old.Content, got.Content, "wording is kept")
	require.Equal(t, old.DisplayText, got.DisplayText)
	require.Equal(t, old.Embedding, got.Embedding, "embedding is kept")
	require.Equal(t, memory.StatusActive, got.Status)
	require.Equal(t, t0, got.LastObservedAt)
	require.Equal(t, t0.Add(days(730)), got.ExpiresAt, "extended to now+730d")
	require.Equal(t, old.FirstObservedAt, got.FirstObservedAt)
	require.Equal(t, uuidFor(901), *got.SourceSessionID)
	require.Equal(t, c.TurnIDs, got.SourceTurnIDs)
	require.Equal(t, uuidFor(77), *got.ExtractionPromptVersionID)
	require.Equal(t, t0, got.UpdatedAt)
	require.Len(t, f.store.All(f.user), 1, "no new row")
	require.Equal(t, []string{"reinforce:" + old.ID.String()}, f.store.Ops())
}

func TestApply_Reinforce_KeepsHigherOldValues(t *testing.T) {
	f := newFixture(t)
	old := f.seed(memory.CategoryPreference, "pref:topic_like:food", base, func(m *memory.Memory) {
		m.Confidence, m.Importance = 0.90, 0.95
		m.ExpiresAt = t0.Add(days(1000)) // longer than any new TTL
	})
	f.apply(cand(memory.CategoryPreference, "pref:topic_like:food", base, func(c *memory.Candidate) {
		c.Confidence, c.Importance = 0.75, 0.10
	}))
	got := f.get(old.ID)
	require.InDelta(t, 0.95, got.Confidence, 1e-9, "max(0.90, 0.75)+0.05")
	require.InDelta(t, 0.95, got.Importance, 1e-9)
	require.Equal(t, t0.Add(days(1000)), got.ExpiresAt, "expires_at never shrinks")
}

func TestApply_Reinforce_CountAccumulatesAndConfidenceCaps(t *testing.T) {
	f := newFixture(t)
	old := f.seed(memory.CategoryPreference, "pref:formality", base, func(m *memory.Memory) { m.Confidence = 0.98 })
	for i := 2; i <= 4; i++ {
		f.apply(cand(memory.CategoryPreference, "pref:formality", base))
		got := f.get(old.ID)
		require.Equal(t, i, got.ObservationCount)
		require.InDelta(t, 0.99, got.Confidence, 1e-9)
	}
}

func TestApply_SlotSimilarityThreshold(t *testing.T) {
	// k=81 is exactly 0.90 (reinforce: >=), k=80 is 0.894 (supersede).
	for _, tc := range []struct {
		name string
		k    int
		want memory.Action
	}{{"exactly 0.90", 81, memory.ActionReinforced}, {"just below", 80, memory.ActionSuperseded}, {"identical", 100, memory.ActionReinforced}} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			f.seed(memory.CategoryProfileFact, "occupation", base)
			require.Equal(t, tc.want, f.apply(cand(memory.CategoryProfileFact, "occupation", vec(0, tc.k))))
		})
	}
}

func TestApply_SameSlotSim05_Supersedes(t *testing.T) {
	f := newFixture(t)
	old := f.seed(memory.CategoryProfileFact, "employer", base)
	c := cand(memory.CategoryProfileFact, "employer", vec(0, 25)) // cosine 0.5
	c.PromptVersionID = ptr(uuidFor(77))

	require.Equal(t, memory.ActionSuperseded, f.apply(c))

	all := f.store.All(f.user)
	require.Len(t, all, 2)
	oldNow := f.get(old.ID)
	require.Equal(t, memory.StatusSuperseded, oldNow.Status)
	require.NotNil(t, oldNow.SupersededBy)
	require.Equal(t, t0.Add(days(30)), oldNow.ExpiresAt)
	require.Equal(t, t0, oldNow.UpdatedAt)
	require.Equal(t, 1, oldNow.ObservationCount, "old row is otherwise untouched")

	fresh := f.get(*oldNow.SupersededBy)
	require.NotEqual(t, old.ID, fresh.ID)
	require.Equal(t, memory.StatusActive, fresh.Status)
	require.Equal(t, 1, fresh.ObservationCount)
	require.Equal(t, t0, fresh.FirstObservedAt)
	require.Equal(t, t0, fresh.LastObservedAt)
	require.Equal(t, t0.Add(days(730)), fresh.ExpiresAt)
	require.Equal(t, "新しい内容", fresh.Content)
	require.Equal(t, "baru", fresh.DisplayText)
	require.Equal(t, vec(0, 25), fresh.Embedding)
	require.Equal(t, "emb-test", fresh.EmbeddingModel)
	require.Equal(t, f.user, fresh.UserID)
	require.Equal(t, uuidFor(901), *fresh.SourceSessionID)
	require.Equal(t, c.TurnIDs, fresh.SourceTurnIDs)
	require.Equal(t, uuidFor(77), *fresh.ExtractionPromptVersionID)
	require.InDelta(t, 0.95, fresh.Confidence, 1e-9)
	require.Equal(t, "employer", fresh.Key)
	require.Len(t, f.active(memory.CategoryProfileFact), 1)

	// the UPDATE precedes the INSERT (ux_memories_active_slot), and the id is the one superseded_by points to
	require.Equal(t, []string{"supersede:" + old.ID.String(), "insert:" + fresh.ID.String()}, f.store.Ops())
}

func TestApply_OldEmbeddingNil_TreatsSimAsZero_Supersedes(t *testing.T) {
	f := newFixture(t)
	old := f.seed(memory.CategoryProfileFact, "employer", nil)
	require.Equal(t, memory.ActionSuperseded, f.apply(cand(memory.CategoryProfileFact, "employer", base)))
	require.Equal(t, memory.StatusSuperseded, f.get(old.ID).Status)
}

func TestApply_CrossKeySim092_ReinforcesOldKey(t *testing.T) {
	f := newFixture(t)
	old := f.seed(memory.CategoryPreference, "pref:topic_like:food", base)
	// the LLM used another key for the same fact; cosine 0.92 (k=85 -> 0.922)
	require.Equal(t, memory.ActionReinforced, f.apply(cand(memory.CategoryPreference, "pref:topic_like:cooking", vec(0, 85))))
	require.Len(t, f.store.All(f.user), 1)
	got := f.get(old.ID)
	require.Equal(t, "pref:topic_like:food", got.Key, "the old key stays")
	require.Equal(t, 2, got.ObservationCount)
	require.InDelta(t, 0.99, got.Confidence, 1e-9)
	require.Equal(t, old.Content, got.Content)
}

func TestApply_CrossKeyThreshold(t *testing.T) {
	for _, tc := range []struct {
		name         string
		seedV, candV []float32
		want         memory.Action
		rows         int
	}{
		{"exactly 0.90", base, vec(0, 81), memory.ActionReinforced, 1},
		{"below 0.90", base, vec(0, 80), memory.ActionCreated, 2},
		{"unrelated", vec(0, 50), vec(50, 60), memory.ActionCreated, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			f.seed(memory.CategoryLearningPattern, "error:particle_wa_ga", tc.seedV)
			require.Equal(t, tc.want, f.apply(cand(memory.CategoryLearningPattern, "error:particle_ni_de", tc.candV)))
			require.Len(t, f.store.All(f.user), tc.rows)
		})
	}
}

func TestApply_CrossKey_OtherCategoryIsIgnored(t *testing.T) {
	f := newFixture(t)
	f.seed(memory.CategoryPreference, "pref:topic_like:food", base)
	require.Equal(t, memory.ActionCreated, f.apply(cand(memory.CategoryLearningPattern, "error:particle_wa_ga", base)))
	require.Len(t, f.store.All(f.user), 2)
}

func TestApply_CrossKey_IgnoresSupersededAndOtherUsers(t *testing.T) {
	f := newFixture(t)
	f.seed(memory.CategoryPreference, "pref:topic_like:food", base, func(m *memory.Memory) {
		m.Status = memory.StatusSuperseded
	})
	other := uuidFor(2)
	f.store.Put(memory.Memory{
		ID: uuidFor(300), UserID: other, Category: memory.CategoryPreference, Key: "pref:topic_like:cooking", Content: "x", DisplayText: "x",
		EvidenceType: memory.EvidenceExplicit, Confidence: 0.9, Importance: 0.5, ObservationCount: 1, Embedding: base,
		Status: memory.StatusActive, FirstObservedAt: t0, LastObservedAt: t0, ExpiresAt: t0.Add(days(10)), CreatedAt: t0, UpdatedAt: t0,
	})
	require.Equal(t, memory.ActionCreated, f.apply(cand(memory.CategoryPreference, "pref:topic_like:travel", base)))
	require.Len(t, f.active(memory.CategoryPreference), 1)
	untouched, _ := f.store.Get(other, uuidFor(300))
	require.Equal(t, 1, untouched.ObservationCount, "another user's memory is not reinforced")
}

func TestApply_CrossKeyEpisodicSim092_Supersedes(t *testing.T) {
	f := newFixture(t)
	oldKey := "episode:" + uuidFor(950).String()
	old := f.seed(memory.CategoryEpisodic, oldKey, base)
	c := cand(memory.CategoryEpisodic, "episode:"+uuidFor(901).String(), vec(0, 85))
	require.Equal(t, memory.ActionSuperseded, f.apply(c))

	oldNow := f.get(old.ID)
	require.Equal(t, memory.StatusSuperseded, oldNow.Status)
	require.Equal(t, t0.Add(days(30)), oldNow.ExpiresAt)
	fresh := f.get(*oldNow.SupersededBy)
	require.Equal(t, c.Key, fresh.Key)
	require.Equal(t, t0.Add(days(180)), fresh.ExpiresAt)
	require.Len(t, f.active(memory.CategoryEpisodic), 1)
}

func TestApply_CrossKeyEpisodicBelow090_Inserts(t *testing.T) {
	f := newFixture(t)
	f.seed(memory.CategoryEpisodic, "episode:"+uuidFor(950).String(), base)
	require.Equal(t, memory.ActionCreated, f.apply(cand(memory.CategoryEpisodic, "episode:"+uuidFor(901).String(), vec(0, 80))))
	require.Len(t, f.active(memory.CategoryEpisodic), 2)
}

func TestApply_Insert(t *testing.T) {
	f := newFixture(t)
	c := cand(memory.CategoryLearningPattern, "error:particle_wa_ga", base)
	require.Equal(t, memory.ActionCreated, f.apply(c))
	all := f.store.All(f.user)
	require.Len(t, all, 1)
	m := all[0]
	require.Equal(t, memory.StatusActive, m.Status)
	require.Equal(t, 1, m.ObservationCount)
	require.Equal(t, t0, m.FirstObservedAt)
	require.Equal(t, t0, m.CreatedAt)
	require.Equal(t, t0.Add(days(180)), m.ExpiresAt)
	require.Equal(t, c.ContentJA, m.Content)
	require.Equal(t, c.DisplayID, m.DisplayText)
	require.Equal(t, c.Embedding, m.Embedding)
	require.Nil(t, m.SupersededBy)
	require.Equal(t, f.user, m.UserID)
	require.Equal(t, uuid.Version(7), m.ID.Version())
}

func TestApply_ActiveTombstone_Rejected_USER_DELETED_SLOT(t *testing.T) {
	f := newFixture(t)
	f.store.Put(tombstone(f.user, uuidFor(400), memory.CategoryProfileFact, "occupation", t0.Add(time.Second)))
	require.Equal(t, memory.ActionRejected, f.apply(cand(memory.CategoryProfileFact, "occupation", base)))
	require.Len(t, f.store.All(f.user), 1, "nothing written")
	require.Empty(t, f.store.Ops())
	require.Equal(t, map[string]float64{memory.ReasonUserDeletedSlot: 1}, rejectedCounts(t, f.reg))
	require.Equal(t, map[string]float64{"rejected": 1}, counterValues(t, f.reg, "kotoki_memory_apply_total", "action"))
}

func TestApply_Tombstone_BoundaryAndScope(t *testing.T) {
	t.Run("expired tombstone allows insert", func(t *testing.T) {
		f := newFixture(t)
		f.store.Put(tombstone(f.user, uuidFor(400), memory.CategoryProfileFact, "occupation", t0)) // expires_at == now: not > now
		require.Equal(t, memory.ActionCreated, f.apply(cand(memory.CategoryProfileFact, "occupation", base)))
		require.Len(t, f.active(memory.CategoryProfileFact), 1)
	})
	t.Run("tombstone expiring after the clock moved", func(t *testing.T) {
		f := newFixture(t)
		f.store.Put(tombstone(f.user, uuidFor(400), memory.CategoryProfileFact, "occupation", t0.Add(days(90))))
		require.Equal(t, memory.ActionRejected, f.apply(cand(memory.CategoryProfileFact, "occupation", base)))
		f.clk.Advance(days(91))
		require.Equal(t, memory.ActionCreated, f.apply(cand(memory.CategoryProfileFact, "occupation", base)))
	})
	t.Run("other key, other category and other user are not blocked", func(t *testing.T) {
		f := newFixture(t)
		f.store.Put(tombstone(f.user, uuidFor(400), memory.CategoryProfileFact, "occupation", t0.Add(days(90))))
		require.Equal(t, memory.ActionCreated, f.apply(cand(memory.CategoryProfileFact, "employer", vec(0, 10))))
		require.Equal(t, memory.ActionCreated, f.apply(cand(memory.CategoryPreference, "occupation", vec(20, 30))))
		require.Equal(t, memory.ActionCreated, f.applyFor(uuidFor(2), cand(memory.CategoryProfileFact, "occupation", base)))
	})
	t.Run("tombstone of another key does not stop a reinforce", func(t *testing.T) {
		f := newFixture(t)
		f.store.Put(tombstone(f.user, uuidFor(400), memory.CategoryPreference, "pref:topic_like:cooking", t0.Add(days(90))))
		f.seed(memory.CategoryPreference, "pref:topic_like:food", base)
		require.Equal(t, memory.ActionReinforced, f.apply(cand(memory.CategoryPreference, "pref:topic_like:food", base)))
	})
}

func tombstone(user, id uuid.UUID, cat memory.Category, key string, expires time.Time) memory.Memory {
	return memory.Memory{
		ID: id, UserID: user, Category: cat, Key: key, EvidenceType: memory.EvidenceUserDeleted, ObservationCount: 1,
		Status: memory.StatusDeleted, FirstObservedAt: t0, LastObservedAt: t0, ExpiresAt: expires, CreatedAt: t0, UpdatedAt: t0,
	}
}

func TestApply_NoEmbedding_Errors(t *testing.T) {
	f := newFixture(t)
	err := f.store.WithinTx(context.Background(), func(ctx context.Context, tx memory.Tx) error {
		_, err := f.ap.Apply(ctx, tx, f.user, cand(memory.CategoryProfileFact, "occupation", nil))
		return err
	})
	require.ErrorIs(t, err, memory.ErrNoEmbedding)
	require.Empty(t, f.store.All(f.user))
}

func TestApply_RepositoryErrorsSurfaceAndRollback(t *testing.T) {
	boom := errors.New("boom")
	for _, method := range []string{"TombstoneActive", "GetActiveSlotForUpdate", "FindNearestActive", "Insert", "Supersede", "Reinforce"} {
		t.Run(method, func(t *testing.T) {
			f := newFixture(t)
			old := f.seed(memory.CategoryProfileFact, "employer", base)
			f.store.FailNext(method, boom)
			c := cand(memory.CategoryProfileFact, "employer", vec(0, 25)) // supersede path
			switch method {
			case "FindNearestActive", "Insert":
				c = cand(memory.CategoryProfileFact, "other", vec(0, 25)) // insert path
			case "Reinforce":
				c = cand(memory.CategoryProfileFact, "employer", base)
			}
			err := f.store.WithinTx(context.Background(), func(ctx context.Context, tx memory.Tx) error {
				_, err := f.ap.Apply(ctx, tx, f.user, c)
				return err
			})
			require.ErrorIs(t, err, boom)
			require.Len(t, f.store.All(f.user), 1, "rolled back")
			require.Equal(t, old, f.get(old.ID))
		})
	}
}

func TestApply_UserIsolation(t *testing.T) {
	f := newFixture(t)
	a, b := uuidFor(1), uuidFor(2)
	require.Equal(t, memory.ActionCreated, f.applyFor(a, cand(memory.CategoryProfileFact, "occupation", base)))
	// the very same slot and embedding for another user is a new memory, not a reinforcement
	require.Equal(t, memory.ActionCreated, f.applyFor(b, cand(memory.CategoryProfileFact, "occupation", base)))
	require.Len(t, f.store.All(a), 1)
	require.Len(t, f.store.All(b), 1)
	require.Equal(t, 1, f.store.All(a)[0].ObservationCount)
	require.Equal(t, 1, f.store.All(b)[0].ObservationCount)
	// and a reinforcement of A leaves B alone
	require.Equal(t, memory.ActionReinforced, f.applyFor(a, cand(memory.CategoryProfileFact, "occupation", base)))
	require.Equal(t, 2, f.store.All(a)[0].ObservationCount)
	require.Equal(t, 1, f.store.All(b)[0].ObservationCount)
}

func TestTTLFor(t *testing.T) {
	now := t0
	for _, tc := range []struct {
		name string
		c    memory.Candidate
		want time.Duration
	}{
		{"permanent fact", memory.Candidate{Category: memory.CategoryProfileFact}, days(730)},
		{"temporary 45", memory.Candidate{Category: memory.CategoryProfileFact, IsTemporary: true, ValidDays: ptr(45)}, days(45)},
		{"temporary default 60", memory.Candidate{Category: memory.CategoryProfileFact, IsTemporary: true}, days(60)},
		{"temporary clamped to 120", memory.Candidate{Category: memory.CategoryProfileFact, IsTemporary: true, ValidDays: ptr(500)}, days(120)},
		{"temporary clamped to 1", memory.Candidate{Category: memory.CategoryProfileFact, IsTemporary: true, ValidDays: ptr(0)}, days(1)},
		{"valid_days ignored when permanent", memory.Candidate{Category: memory.CategoryProfileFact, ValidDays: ptr(45)}, days(730)},
		{"preference", memory.Candidate{Category: memory.CategoryPreference}, days(365)},
		{"learning pattern", memory.Candidate{Category: memory.CategoryLearningPattern}, days(180)},
		{"episodic", memory.Candidate{Category: memory.CategoryEpisodic}, days(180)},
	} {
		t.Run(tc.name, func(t *testing.T) { require.Equal(t, now.Add(tc.want), memory.TTLFor(tc.c, now)) })
	}
}

func TestApply_TemporaryFactUsesCandidateValidDays(t *testing.T) {
	f := newFixture(t)
	f.apply(cand(memory.CategoryProfileFact, "plan:job_interview", base, func(c *memory.Candidate) {
		c.IsTemporary, c.ValidDays = true, ptr(45)
	}))
	require.Equal(t, t0.Add(days(45)), f.active(memory.CategoryProfileFact)[0].ExpiresAt)
}

func TestReinforcedConfidence(t *testing.T) {
	require.InDelta(t, 0.95, memory.ReinforcedConfidence(0.90, 0.75), 1e-12)
	require.InDelta(t, 0.95, memory.ReinforcedConfidence(0.75, 0.90), 1e-12)
	require.InDelta(t, 0.80, memory.ReinforcedConfidence(0.70, 0.75), 1e-12)
	require.InDelta(t, 0.99, memory.ReinforcedConfidence(0.95, 0.95), 1e-12)
	require.InDelta(t, 0.99, memory.ReinforcedConfidence(0.99, 0.99), 1e-12)
}

func TestCosine(t *testing.T) {
	require.InDelta(t, 1, memory.Cosine([]float32{1, 2}, []float32{2, 4}), 1e-9)
	require.InDelta(t, 0, memory.Cosine([]float32{1, 0}, []float32{0, 1}), 1e-9)
	require.InDelta(t, -1, memory.Cosine([]float32{1, 0}, []float32{-1, 0}), 1e-9)
	require.Zero(t, memory.Cosine([]float32{1, 0}, []float32{1, 0, 0}), "dimension mismatch")
	require.Zero(t, memory.Cosine(nil, nil))
	require.Zero(t, memory.Cosine([]float32{0, 0}, []float32{1, 0}), "zero vector")
	require.Zero(t, memory.Cosine([]float32{1, 0}, []float32{0, 0}), "zero vector")
}

// fill stores n ACTIVE memories of cat with distinct keys; score i (0-based) gives importance and
// confidence from mod.
func (f *fixture) fill(cat memory.Category, n int, mod func(i int, m *memory.Memory)) []memory.Memory {
	out := make([]memory.Memory, 0, n)
	for i := 0; i < n; i++ {
		m := f.seed(cat, fmt.Sprintf("hobby:h%d", i), vec(i%dim, i%dim+1), func(m *memory.Memory) {
			m.ID = uuidFor(1000 + i)
			if cat == memory.CategoryLearningPattern {
				m.Key = fmt.Sprintf("error:e%d", i)
			}
			if cat == memory.CategoryEpisodic {
				m.Key = "episode:" + uuidFor(2000+i).String()
			}
			mod(i, m)
		})
		out = append(out, m)
	}
	return out
}

func (f *fixture) enforce() {
	f.t.Helper()
	require.NoError(f.t, f.store.WithinTx(context.Background(), func(ctx context.Context, tx memory.Tx) error {
		return f.ap.EnforceCaps(ctx, tx, f.user)
	}))
}

func TestEnforceCaps_FactPreference(t *testing.T) {
	f := newFixture(t)
	// 101 rows (mixed categories): equal scores except row 37 (lowest 0.6*imp+0.4*conf)
	rows := f.fill(memory.CategoryProfileFact, 101, func(i int, m *memory.Memory) {
		m.Importance, m.Confidence = 0.6, 0.8
		if i%2 == 1 {
			m.Category = memory.CategoryPreference
		}
		if i == 37 {
			m.Importance, m.Confidence = 0.2, 0.9
		}
	})
	f.enforce()
	require.Len(t, f.store.All(f.user), 100)
	_, gone := f.store.Get(f.user, rows[37].ID)
	require.False(t, gone, "lowest 0.6*importance+0.4*confidence is deleted (0.48 vs 0.68)")
}

func TestEnforceCaps_ScoreWeights(t *testing.T) {
	// importance weighs 0.6 and confidence 0.4: A (imp .9, conf .1 = .58) beats B (imp .1, conf .9 = .42)
	f := newFixture(t)
	rows := f.fill(memory.CategoryProfileFact, 101, func(_ int, m *memory.Memory) { m.Importance, m.Confidence = 0.7, 0.7 })
	set := func(i int, imp, conf float64) {
		m := rows[i]
		m.Importance, m.Confidence = imp, conf
		f.store.Put(m)
	}
	set(5, 0.9, 0.1)
	set(6, 0.1, 0.9)
	f.enforce()
	_, aOK := f.store.Get(f.user, rows[5].ID)
	_, bOK := f.store.Get(f.user, rows[6].ID)
	require.True(t, aOK)
	require.False(t, bOK)
}

func TestEnforceCaps_TieDeletesOldestObservation(t *testing.T) {
	f := newFixture(t)
	rows := f.fill(memory.CategoryLearningPattern, 41, func(i int, m *memory.Memory) {
		m.Importance, m.Confidence = 0.7, 0.8
		m.LastObservedAt = t0.Add(-time.Duration(i) * time.Hour) // row 40 is the oldest
		m.FirstObservedAt = m.LastObservedAt
	})
	f.enforce()
	require.Len(t, f.store.All(f.user), 40)
	_, gone := f.store.Get(f.user, rows[40].ID)
	require.False(t, gone)
}

func TestEnforceCaps_Episodic(t *testing.T) {
	f := newFixture(t)
	rows := f.fill(memory.CategoryEpisodic, 201, func(i int, m *memory.Memory) {
		m.Importance, m.Confidence = 0.1, 0.1 // score is irrelevant for episodes
		if i == 200 {
			m.Importance, m.Confidence = 1, 1
		}
		m.LastObservedAt = t0.Add(-time.Duration(i) * time.Hour)
		m.FirstObservedAt = m.LastObservedAt
	})
	// row 200 has the oldest observation and the HIGHEST score; it must still go
	f.enforce()
	require.Len(t, f.store.All(f.user), 200)
	_, gone := f.store.Get(f.user, rows[200].ID)
	require.False(t, gone)
}

func TestEnforceCaps_AtLimitAndGroupsAreIndependent(t *testing.T) {
	f := newFixture(t)
	f.fill(memory.CategoryProfileFact, 100, func(int, *memory.Memory) {})
	f.enforce()
	require.Len(t, f.store.All(f.user), 100, "exactly at the cap deletes nothing")

	// 40 patterns and 200 episodes fit next to 100 facts/preferences
	f2 := newFixture(t)
	f2.fill(memory.CategoryProfileFact, 100, func(int, *memory.Memory) {})
	f2.fill(memory.CategoryLearningPattern, 40, func(i int, m *memory.Memory) { m.ID = uuidFor(5000 + i) })
	f2.fill(memory.CategoryEpisodic, 200, func(i int, m *memory.Memory) { m.ID = uuidFor(6000 + i) })
	f2.enforce()
	require.Len(t, f2.store.All(f2.user), 340)
}

func TestEnforceCaps_OnlyActiveCountAndOtherUsersUntouched(t *testing.T) {
	f := newFixture(t)
	f.fill(memory.CategoryProfileFact, 100, func(int, *memory.Memory) {})
	// SUPERSEDED rows and tombstones do not count toward the cap
	f.seed(memory.CategoryProfileFact, "old:one", base, func(m *memory.Memory) { m.ID = uuidFor(7001); m.Status = memory.StatusSuperseded })
	f.store.Put(tombstone(f.user, uuidFor(7002), memory.CategoryProfileFact, "gone", t0.Add(days(10))))
	// another user is over the cap but is not f.user
	other := uuidFor(2)
	for i := 0; i < 101; i++ {
		f.seed(memory.CategoryPreference, fmt.Sprintf("pref:topic_like:t%d", i), nil, func(m *memory.Memory) { m.ID = uuidFor(8000 + i); m.UserID = other })
	}
	f.enforce()
	require.Len(t, f.store.All(f.user), 102, "own: 100 active + superseded + tombstone")
	require.Len(t, f.store.All(other), 101, "another user's rows are not capped by this call")
}

func TestApplyAll_OrderMetricsAndCaps(t *testing.T) {
	f := newFixture(t)
	// 100 facts already (low scores): the batch adds two facts and one preference, so three are cut
	f.fill(memory.CategoryProfileFact, 100, func(_ int, m *memory.Memory) { m.Importance, m.Confidence = 0.5, 0.5 })
	f.seed(memory.CategoryLearningPattern, "error:particle_wa_ga", vec(40, 100), func(m *memory.Memory) { m.ID = uuidFor(9001) })

	cands := []memory.Candidate{
		cand(memory.CategoryEpisodic, "episode:"+uuidFor(901).String(), vec(0, 3)),
		cand(memory.CategoryLearningPattern, "error:particle_wa_ga", vec(40, 100)), // reinforces
		cand(memory.CategoryPreference, "pref:formality", vec(10, 12), func(c *memory.Candidate) { c.Confidence = 0.71 }),
		cand(memory.CategoryProfileFact, "occupation", vec(20, 23), func(c *memory.Candidate) { c.Confidence = 0.80 }),
		cand(memory.CategoryProfileFact, "employer", vec(30, 34), func(c *memory.Candidate) { c.Confidence = 0.99 }),
	}
	var acts []memory.Action
	require.NoError(t, f.store.WithinTx(context.Background(), func(ctx context.Context, tx memory.Tx) error {
		var err error
		acts, err = f.ap.ApplyAll(ctx, tx, f.user, cands)
		return err
	}))
	require.Equal(t, []memory.Action{
		memory.ActionCreated, memory.ActionCreated, memory.ActionCreated, // employer (0.99), occupation (0.80), preference
		memory.ActionReinforced, memory.ActionCreated, // pattern, episode
	}, acts)
	require.Equal(t, map[string]float64{"created": 4, "reinforced": 1}, counterValues(t, f.reg, "kotoki_memory_apply_total", "action"))

	// insertion order follows the category order, confidence descending
	var inserted []string
	for _, op := range f.store.Ops() {
		if len(op) > 7 && op[:7] == "insert:" {
			inserted = append(inserted, op)
		}
	}
	keys := map[string]string{}
	for _, m := range f.store.All(f.user) {
		keys["insert:"+m.ID.String()] = m.Key
	}
	var order []string
	for _, op := range inserted {
		order = append(order, keys[op])
	}
	require.Equal(t, []string{"employer", "occupation", "pref:formality", "episode:" + uuidFor(901).String()}, order)

	// caps ran in the same transaction: 103 facts/preferences -> 100
	n := 0
	for _, m := range f.store.All(f.user) {
		if m.Status == memory.StatusActive && (m.Category == memory.CategoryProfileFact || m.Category == memory.CategoryPreference) {
			n++
		}
	}
	require.Equal(t, 100, n)
}

func TestApplyAll_LocksUserFirstAndFailureRollsBack(t *testing.T) {
	f := newFixture(t)
	boom := errors.New("lock failed")
	f.store.FailNext("LockUser", boom)
	err := f.store.WithinTx(context.Background(), func(ctx context.Context, tx memory.Tx) error {
		_, err := f.ap.ApplyAll(ctx, tx, f.user, []memory.Candidate{cand(memory.CategoryProfileFact, "occupation", base)})
		return err
	})
	require.ErrorIs(t, err, boom)
	require.Empty(t, f.store.All(f.user), "nothing is written before the lock")

	// a failure on the second candidate undoes the first
	err = f.store.WithinTx(context.Background(), func(ctx context.Context, tx memory.Tx) error {
		_, err := f.ap.ApplyAll(ctx, tx, f.user, []memory.Candidate{
			cand(memory.CategoryProfileFact, "occupation", base),
			cand(memory.CategoryPreference, "pref:formality", nil), // no embedding
		})
		return err
	})
	require.ErrorIs(t, err, memory.ErrNoEmbedding)
	require.Empty(t, f.store.All(f.user))
}

func TestApplyAll_DoesNotMutateInput(t *testing.T) {
	f := newFixture(t)
	cands := []memory.Candidate{
		cand(memory.CategoryEpisodic, "episode:"+uuidFor(901).String(), vec(0, 3)),
		cand(memory.CategoryProfileFact, "occupation", vec(20, 23)),
	}
	require.NoError(t, f.store.WithinTx(context.Background(), func(ctx context.Context, tx memory.Tx) error {
		_, err := f.ap.ApplyAll(ctx, tx, f.user, cands)
		return err
	}))
	require.Equal(t, memory.CategoryEpisodic, cands[0].Category)
}
