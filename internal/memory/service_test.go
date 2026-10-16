package memory_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/memory"
	"github.com/nabePi/kotoki-backend/internal/platform/clock"
	"github.com/nabePi/kotoki-backend/internal/platform/errs"
	"github.com/nabePi/kotoki-backend/internal/testutil/fakes"
)

var svcNow = time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)

type svcRig struct {
	store *fakes.MemoryStore
	ver   *fakes.MemVerRecorder
	prof  *fakes.MemoryProfile
	svc   *memory.Service
}

func newSvcRig() *svcRig {
	r := &svcRig{
		store: fakes.NewMemoryStore(fakes.WithMemoryClock(clock.NewFake(svcNow))),
		ver:   fakes.NewMemVerRecorder(), prof: fakes.NewMemoryProfile(),
	}
	r.svc = memory.NewService(r.store, r.ver, r.prof, clock.NewFake(svcNow), nil)
	return r
}

func (r *svcRig) put(user uuid.UUID, cat memory.Category, key, status string, observed time.Time, expires time.Time) memory.Memory {
	m := memory.Memory{
		ID: uuid.Must(uuid.NewV7()), UserID: user, Category: cat, Key: key, Content: "内容", DisplayText: "isi",
		EvidenceType: memory.EvidenceExplicit, Confidence: 0.9, Importance: 0.5, ObservationCount: 1, Status: status,
		FirstObservedAt: observed, LastObservedAt: observed, ExpiresAt: expires, CreatedAt: observed, UpdatedAt: observed,
	}
	r.store.Put(m)
	return m
}

func TestDelete_BumpsMemverAndTombstones(t *testing.T) {
	r := newSvcRig()
	u := uuid.Must(uuid.NewV7())
	m := r.put(u, memory.CategoryProfileFact, "job", memory.StatusActive, svcNow.Add(-time.Hour), svcNow.Add(24*time.Hour))
	old := r.put(u, memory.CategoryProfileFact, "job", memory.StatusSuperseded, svcNow.Add(-48*time.Hour), svcNow.Add(24*time.Hour))
	r.store.Put(withSupersededBy(old, m.ID))
	require.NoError(t, r.svc.Delete(context.Background(), u, m.ID))

	_, ok := r.store.Get(u, m.ID)
	require.False(t, ok, "the row is hard-deleted")
	var tombstones int
	for _, row := range r.store.All(u) {
		if row.Status == memory.StatusDeleted {
			tombstones++
			require.Equal(t, memory.CategoryProfileFact, row.Category)
			require.Equal(t, "job", row.Key)
			require.Empty(t, row.Content)
			require.Empty(t, row.DisplayText)
			require.True(t, row.ExpiresAt.Equal(svcNow.Add(90*24*time.Hour)), "90 days")
		}
	}
	require.Equal(t, 1, tombstones)
	require.Len(t, r.store.All(u), 1, "the superseded chain behind the memory is gone too")
	require.Equal(t, int64(1), r.ver.Version(u))
	require.Equal(t, [][]uuid.UUID{{u}}, r.ver.Bumps())
}

func TestDelete_EpisodicLeavesNoTombstone(t *testing.T) {
	r := newSvcRig()
	u := uuid.Must(uuid.NewV7())
	m := r.put(u, memory.CategoryEpisodic, "episode:"+uuid.NewString(), memory.StatusActive, svcNow.Add(-time.Hour), svcNow.Add(24*time.Hour))
	require.NoError(t, r.svc.Delete(context.Background(), u, m.ID))
	require.Empty(t, r.store.All(u))
	require.Equal(t, int64(1), r.ver.Version(u))
}

func TestDelete_NotFoundCases(t *testing.T) {
	r := newSvcRig()
	a, b := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	other := r.put(b, memory.CategoryPreference, "k", memory.StatusActive, svcNow, svcNow.Add(time.Hour))
	superseded := r.put(a, memory.CategoryPreference, "k2", memory.StatusSuperseded, svcNow, svcNow.Add(time.Hour))
	tomb := r.put(a, memory.CategoryPreference, "k3", memory.StatusDeleted, svcNow, svcNow.Add(time.Hour))
	for name, id := range map[string]uuid.UUID{"other user": other.ID, "superseded": superseded.ID, "tombstone": tomb.ID, "unknown": uuid.Must(uuid.NewV7())} {
		err := r.svc.Delete(context.Background(), a, id)
		require.Equal(t, errs.CodeMemoryNotFound, errs.CodeOf(err), name)
	}
	_, ok := r.store.Get(b, other.ID)
	require.True(t, ok, "the row of the other user stays")
	require.Zero(t, r.ver.Version(a), "no bump without a deletion")
	require.Zero(t, r.ver.Version(b))
}

func TestDelete_BumpFailureDoesNotFailCommittedDeletion(t *testing.T) {
	r := newSvcRig()
	u := uuid.Must(uuid.NewV7())
	m := r.put(u, memory.CategoryPreference, "k", memory.StatusActive, svcNow, svcNow.Add(time.Hour))
	r.ver.FailNext("Bump", errors.New("redis down"))
	require.NoError(t, r.svc.Delete(context.Background(), u, m.ID))
	_, ok := r.store.Get(u, m.ID)
	require.False(t, ok)
}

func TestDeleteAll_Idempotent_NoTombstone(t *testing.T) {
	r := newSvcRig()
	a, b := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	r.put(a, memory.CategoryProfileFact, "job", memory.StatusActive, svcNow, svcNow.Add(time.Hour))
	r.put(a, memory.CategoryProfileFact, "job", memory.StatusSuperseded, svcNow, svcNow.Add(time.Hour))
	r.put(a, memory.CategoryPreference, "x", memory.StatusDeleted, svcNow, svcNow.Add(time.Hour))
	r.put(a, memory.CategoryEpisodic, "episode:"+uuid.NewString(), memory.StatusActive, svcNow, svcNow.Add(time.Hour))
	keep := r.put(b, memory.CategoryProfileFact, "job", memory.StatusActive, svcNow, svcNow.Add(time.Hour))

	require.NoError(t, r.svc.DeleteAll(context.Background(), a))
	require.Empty(t, r.store.All(a), "every row of the user, tombstones included, and no new tombstone")
	require.NoError(t, r.svc.DeleteAll(context.Background(), a), "again: still success")
	require.Empty(t, r.store.All(a))
	_, ok := r.store.Get(b, keep.ID)
	require.True(t, ok)
	require.Equal(t, int64(2), r.ver.Version(a))
	require.Zero(t, r.ver.Version(b))
}

func TestList_FiltersExpiredAndNonActive(t *testing.T) {
	r := newSvcRig()
	u, o := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	live := r.put(u, memory.CategoryProfileFact, "a", memory.StatusActive, svcNow.Add(-time.Hour), svcNow.Add(time.Hour))
	r.put(u, memory.CategoryProfileFact, "b", memory.StatusActive, svcNow.Add(-time.Hour), svcNow.Add(-time.Second))
	r.put(u, memory.CategoryProfileFact, "c", memory.StatusSuperseded, svcNow.Add(-time.Hour), svcNow.Add(time.Hour))
	r.put(u, memory.CategoryProfileFact, "d", memory.StatusDeleted, svcNow.Add(-time.Hour), svcNow.Add(time.Hour))
	r.put(o, memory.CategoryProfileFact, "a", memory.StatusActive, svcNow, svcNow.Add(time.Hour))

	res, err := r.svc.List(context.Background(), u, nil, nil, 0)
	require.NoError(t, err)
	require.Len(t, res.Items, 1)
	require.Equal(t, live.ID, res.Items[0].ID)
	require.Nil(t, res.Next)
	require.True(t, res.MemoryEnabled)
	require.Empty(t, res.Items[0].Embedding)
}

func TestList_PagingAndMemoryEnabled(t *testing.T) {
	r := newSvcRig()
	u := uuid.Must(uuid.NewV7())
	var want []uuid.UUID
	for i := 0; i < 5; i++ {
		m := r.put(u, memory.CategoryPreference, "k"+string(rune('a'+i)), memory.StatusActive, svcNow.Add(-time.Duration(i)*time.Hour), svcNow.Add(time.Hour))
		want = append(want, m.ID) // newest observation first
	}
	r.prof.Disable(u)

	var got []uuid.UUID
	var cur *memory.ListCursor
	pages := 0
	for {
		res, err := r.svc.List(context.Background(), u, nil, cur, 2)
		require.NoError(t, err)
		require.False(t, res.MemoryEnabled)
		pages++
		for _, it := range res.Items {
			got = append(got, it.ID)
		}
		if res.Next == nil {
			break
		}
		require.Len(t, res.Items, 2)
		cur = res.Next
	}
	require.Equal(t, 3, pages)
	require.Equal(t, want, got)
}

func TestList_Validation(t *testing.T) {
	r := newSvcRig()
	u := uuid.Must(uuid.NewV7())
	bad := memory.Category("NOPE")
	_, err := r.svc.List(context.Background(), u, &bad, nil, 10)
	require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
	_, err = r.svc.List(context.Background(), u, nil, &memory.ListCursor{Category: "NOPE", ID: uuid.New(), LastObservedAt: svcNow}, 10)
	require.Equal(t, errs.CodeInvalidCursor, errs.CodeOf(err))
	_, err = r.svc.List(context.Background(), u, nil, &memory.ListCursor{Category: memory.CategoryEpisodic}, 10)
	require.Equal(t, errs.CodeInvalidCursor, errs.CodeOf(err))
}

func withSupersededBy(m memory.Memory, by uuid.UUID) memory.Memory {
	m.SupersededBy = &by
	return m
}
