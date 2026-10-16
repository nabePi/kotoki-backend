//go:build integration

package postgres_test

import (
	"context"
	"encoding/json"
	"math/rand/v2"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/memory"
	"github.com/nabePi/kotoki-backend/internal/platform/clock"
	"github.com/nabePi/kotoki-backend/internal/platform/idgen"
	"github.com/nabePi/kotoki-backend/internal/postgres"
	"github.com/nabePi/kotoki-backend/internal/postgres/sqlcgen"
	"github.com/nabePi/kotoki-backend/internal/testutil"
	"github.com/nabePi/kotoki-backend/internal/testutil/repotest"
)

var memAt = time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC)

type pgMemory struct {
	pool    *pgxpool.Pool
	store   *postgres.Store
	content testutil.ContentIDs
	n       atomic.Int64
}

func newPGMemory(t *testing.T) *pgMemory {
	t.Helper()
	pool := testutil.NewPostgres(t)
	return &pgMemory{pool: pool, store: postgres.NewStore(pool), content: testutil.SeedContent(t, pool)}
}

func (p *pgMemory) user(t *testing.T) uuid.UUID { return testutil.SeedUser(t, p.pool, nil) }

// session inserts a terminal session of userID (ux_sessions_one_open_per_user allows many).
func (p *pgMemory) session(t *testing.T, userID uuid.UUID) uuid.UUID {
	t.Helper()
	snap, err := json.Marshal(map[string]any{
		"character": map[string]any{"id": p.content.CharacterID, "voice": map[string]any{"gender": "FEMALE", "accent": "KANTO"}},
		"formality": "POLITE",
	})
	require.NoError(t, err)
	ended := time.Now().UTC().Add(-time.Duration(p.n.Add(1)) * time.Minute)
	started := ended.Add(-400 * time.Second)
	return testutil.SeedSession(t, p.pool, userID, p.content, testutil.Row{
		"status": "COMPLETED", "end_reason": "USER_ENDED", "starting_at": started.Add(-2 * time.Second), "started_at": started,
		"expires_at": started.Add(300 * time.Second), "ending_at": ended, "ended_at": ended, "charged_seconds": 60, "config_snapshot": snap,
	})
}

// dump reads every memories row of userID by plain SQL, whatever its status, ordered by id.
func (p *pgMemory) dump(t *testing.T, userID uuid.UUID) []memory.Memory {
	t.Helper()
	rows, err := p.pool.Query(context.Background(), `
		SELECT id, user_id, category, memory_key, content, display_text, evidence_type, confidence::float8, importance::float8,
		       observation_count, source_session_id, source_turn_ids, extraction_prompt_version_id, COALESCE(embedding_model, ''),
		       status, superseded_by, first_observed_at, last_observed_at, expires_at, created_at, updated_at
		  FROM memories WHERE user_id = $1 ORDER BY id`, userID)
	require.NoError(t, err)
	defer rows.Close()
	var out []memory.Memory
	for rows.Next() {
		var m memory.Memory
		var cat string
		require.NoError(t, rows.Scan(&m.ID, &m.UserID, &cat, &m.Key, &m.Content, &m.DisplayText, &m.EvidenceType, &m.Confidence, &m.Importance,
			&m.ObservationCount, &m.SourceSessionID, &m.SourceTurnIDs, &m.ExtractionPromptVersionID, &m.EmbeddingModel,
			&m.Status, &m.SupersededBy, &m.FirstObservedAt, &m.LastObservedAt, &m.ExpiresAt, &m.CreatedAt, &m.UpdatedAt))
		m.Category = memory.Category(cat)
		m.FirstObservedAt, m.LastObservedAt, m.ExpiresAt = m.FirstObservedAt.UTC(), m.LastObservedAt.UTC(), m.ExpiresAt.UTC()
		m.CreatedAt, m.UpdatedAt = m.CreatedAt.UTC(), m.UpdatedAt.UTC()
		out = append(out, m)
	}
	require.NoError(t, rows.Err())
	return out
}

func (p *pgMemory) harness() repotest.MemoryHarness {
	return repotest.MemoryHarness{
		Store: p.store.MemoryStore(), NewUser: p.user, NewSession: p.session,
		PromptVersionID: p.content.PromptVersionID, Dump: p.dump,
	}
}

// TestPGMemoryRepo runs the contract shared with the fake (repotest.MemoryRepoSuite) on pgvector.
func TestPGMemoryRepo(t *testing.T) {
	repotest.MemoryRepoSuite(t, func(t *testing.T) repotest.MemoryHarness { return newPGMemory(t).harness() })
}

func unit(x, y float32) []float32 {
	v := make([]float32, 1536)
	v[0], v[1] = x, y
	return v
}

func (p *pgMemory) insert(t *testing.T, m memory.Memory) error {
	t.Helper()
	return p.store.MemoryStore().WithinTx(context.Background(), func(ctx context.Context, tx memory.Tx) error {
		return tx.Memories().Insert(ctx, m.UserID, m)
	})
}

func pgMem(user uuid.UUID, cat memory.Category, key string, mod ...func(*memory.Memory)) memory.Memory {
	m := memory.Memory{
		ID: uuid.Must(uuid.NewV7()), UserID: user, Category: cat, Key: key, Content: "内容", DisplayText: "isi",
		EvidenceType: memory.EvidenceExplicit, Confidence: 0.9, Importance: 0.5, ObservationCount: 1,
		Embedding: unit(1, 0), EmbeddingModel: "m", Status: memory.StatusActive,
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

func TestChkMemoriesContent_RejectsTombstoneWithContent(t *testing.T) {
	p := newPGMemory(t)
	u := p.user(t)
	tomb := pgMem(u, memory.CategoryPreference, "hobby", func(m *memory.Memory) {
		m.Status, m.EvidenceType, m.Embedding, m.EmbeddingModel = memory.StatusDeleted, memory.EvidenceUserDeleted, nil, ""
		m.Confidence, m.Importance = 0, 0
		// Content and DisplayText stay filled: a tombstone keeps no content
	})
	testutil.RequirePgErr(t, p.insert(t, tomb), "23514", "chk_memories_content")

	tomb.Content, tomb.DisplayText = "", ""
	tomb.Embedding = unit(1, 0)
	testutil.RequirePgErr(t, p.insert(t, tomb), "23514", "chk_memories_content")

	tomb.Embedding = nil
	require.NoError(t, p.insert(t, tomb), "a clean tombstone is accepted")

	// the reverse: a live memory needs content
	live := pgMem(u, memory.CategoryPreference, "pet", func(m *memory.Memory) { m.Content = "" })
	testutil.RequirePgErr(t, p.insert(t, live), "23514", "chk_memories_content")
}

func TestActiveSlotUnique(t *testing.T) {
	p := newPGMemory(t)
	u := p.user(t)
	require.NoError(t, p.insert(t, pgMem(u, memory.CategoryProfileFact, "occupation")))
	testutil.RequirePgErr(t, p.insert(t, pgMem(u, memory.CategoryProfileFact, "occupation")), "23505", "ux_memories_active_slot")

	tomb := func() memory.Memory {
		return pgMem(u, memory.CategoryPreference, "hobby", func(m *memory.Memory) {
			m.Status, m.EvidenceType, m.Content, m.DisplayText, m.Embedding, m.EmbeddingModel = memory.StatusDeleted, memory.EvidenceUserDeleted, "", "", nil, ""
			m.Confidence, m.Importance = 0, 0
		})
	}
	require.NoError(t, p.insert(t, tomb()))
	testutil.RequirePgErr(t, p.insert(t, tomb()), "23505", "ux_memories_tombstone_slot")
}

func TestSupersedeDeferredFK_UpdateBeforeInsertInOneTx(t *testing.T) {
	p := newPGMemory(t)
	u := p.user(t)
	old := pgMem(u, memory.CategoryProfileFact, "occupation")
	require.NoError(t, p.insert(t, old))
	next := pgMem(u, memory.CategoryProfileFact, "occupation")
	err := p.store.MemoryStore().WithinTx(context.Background(), func(ctx context.Context, tx memory.Tx) error {
		r := tx.Memories()
		// superseded_by points at a row that does not exist yet: the FK is checked at COMMIT
		if err := r.Supersede(ctx, u, old.ID, next.ID, memAt.Add(30*24*time.Hour), memAt); err != nil {
			return err
		}
		return r.Insert(ctx, u, next)
	})
	require.NoError(t, err, "valid at COMMIT")

	// without the insert, the same UPDATE fails when the transaction commits
	other := pgMem(u, memory.CategoryPreference, "hobby")
	require.NoError(t, p.insert(t, other))
	err = p.store.MemoryStore().WithinTx(context.Background(), func(ctx context.Context, tx memory.Tx) error {
		return tx.Memories().Supersede(ctx, u, other.ID, uuid.Must(uuid.NewV7()), memAt.Add(time.Hour), memAt)
	})
	testutil.RequirePgErr(t, err, "23503", "")
}

func TestNoVectorIndexOnMemories(t *testing.T) {
	p := newPGMemory(t)
	var n int
	require.NoError(t, p.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM pg_indexes WHERE tablename = 'memories' AND indexdef ~* '(hnsw|ivfflat|vector_)'`).Scan(&n))
	require.Zero(t, n, "exact scan per user: no ANN index (spec 11.3.1)")
}

func TestVectorRoundTrip1536(t *testing.T) {
	p := newPGMemory(t)
	u := p.user(t)
	rng := rand.New(rand.NewPCG(1, 2))
	v := make([]float32, 1536)
	for i := range v {
		v[i] = rng.Float32()*2 - 1
	}
	m := pgMem(u, memory.CategoryPreference, "dense", func(m *memory.Memory) { m.Embedding = v })
	require.NoError(t, p.insert(t, m))
	rows, err := p.store.MemoryStore().Reader().ListCore(context.Background(), u, memAt)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, v, rows[0].Embedding, "float32 values survive pgvector exactly")

	ep := pgMem(u, memory.CategoryEpisodic, "episode:1", func(m *memory.Memory) { m.Embedding = v })
	require.NoError(t, p.insert(t, ep))
	hits, err := p.store.MemoryStore().Reader().SearchEpisodic(context.Background(), u, v, 0.99, 3, nil, memAt)
	require.NoError(t, err)
	require.Len(t, hits, 1)
	require.InDelta(t, 1.0, hits[0].Similarity, 1e-6)
	var wide = make([]float32, 1535)
	_, err = p.store.MemoryStore().Reader().SearchEpisodic(context.Background(), u, wide, 0.5, 3, nil, memAt)
	require.Error(t, err, "a query vector of another width is an error, not a silent miss")
}

func (p *pgMemory) applier(at time.Time) *memory.Applier {
	return memory.NewApplier(idgen.NewV7(), clock.NewFake(at), nil)
}

func (p *pgMemory) apply(t *testing.T, a *memory.Applier, user uuid.UUID, c memory.Candidate) memory.Action {
	t.Helper()
	var act memory.Action
	require.NoError(t, p.store.MemoryStore().WithinTx(context.Background(), func(ctx context.Context, tx memory.Tx) error {
		acts, err := a.ApplyAll(ctx, tx, user, []memory.Candidate{c})
		if err != nil {
			return err
		}
		act = acts[0]
		return nil
	}))
	return act
}

func cand(cat memory.Category, key string, v []float32, session uuid.UUID) memory.Candidate {
	return memory.Candidate{
		Category: cat, Key: key, ContentJA: "内容", DisplayID: "isi", EvidenceType: memory.EvidenceExplicit,
		Confidence: 0.9, Importance: 0.5, Embedding: v, EmbeddingModel: "m", SessionID: session,
	}
}

// TestSearchEpisodic_Isolation is spec 11.10: users A and B hold episodes with identical embeddings;
// a search as A returns A's only, as a user without memories nothing (Review Focus 1).
func TestSearchEpisodic_Isolation(t *testing.T) {
	p := newPGMemory(t)
	a, b, c := p.user(t), p.user(t), p.user(t)
	ea := pgMem(a, memory.CategoryEpisodic, "episode:a", func(m *memory.Memory) { m.ID = uuid.UUID{15: 2} })
	eb := pgMem(b, memory.CategoryEpisodic, "episode:b", func(m *memory.Memory) { m.ID = uuid.UUID{15: 1} }) // sorts first: an unscoped search returns it first
	require.NoError(t, p.insert(t, ea))
	require.NoError(t, p.insert(t, eb))
	rd := p.store.MemoryStore().Reader()
	search := func(u uuid.UUID) []memory.EpisodicRow {
		rows, err := rd.SearchEpisodic(context.Background(), u, unit(1, 0), 0.75, 3, nil, memAt)
		require.NoError(t, err)
		return rows
	}
	got := search(a)
	require.Len(t, got, 1)
	require.Equal(t, ea.ID, got[0].ID)
	got = search(b)
	require.Len(t, got, 1)
	require.Equal(t, eb.ID, got[0].ID)
	require.Empty(t, search(c))
}

// TestFindNearestActive_Isolation drives the real Applier and repository: the dedup of A never
// reinforces or supersedes a memory of B, even when the embeddings are identical.
func TestFindNearestActive_Isolation(t *testing.T) {
	p := newPGMemory(t)
	a, b, c := p.user(t), p.user(t), p.user(t)
	sa, sc := p.session(t, a), p.session(t, c)
	ap := p.applier(memAt)

	bFact := pgMem(b, memory.CategoryProfileFact, "occupation")
	bEp := pgMem(b, memory.CategoryEpisodic, "episode:b")
	require.NoError(t, p.insert(t, bFact))
	require.NoError(t, p.insert(t, bEp))
	bBefore := p.dump(t, b)

	// same slot, identical embedding: A gets its own row; B's is not reinforced
	require.Equal(t, memory.ActionCreated, p.apply(t, ap, a, cand(memory.CategoryProfileFact, "occupation", unit(1, 0), sa)))
	// another key, identical embedding (cross-key nearest, sim 1.0): reinforces A's row, never B's
	require.Equal(t, memory.ActionReinforced, p.apply(t, ap, a, cand(memory.CategoryProfileFact, "employer", unit(1, 0), sa)))
	// an episode with the identical embedding: A creates, nothing of B is superseded
	require.Equal(t, memory.ActionCreated, p.apply(t, ap, a, cand(memory.CategoryEpisodic, "episode:a1", unit(1, 0), sa)))
	require.Equal(t, memory.ActionSuperseded, p.apply(t, ap, a, cand(memory.CategoryEpisodic, "episode:a2", unit(1, 0), sa)), "A's own episode is the nearest")
	// a user with nothing creates
	require.Equal(t, memory.ActionCreated, p.apply(t, ap, c, cand(memory.CategoryProfileFact, "employer", unit(1, 0), sc)))

	require.Equal(t, bBefore, p.dump(t, b), "B is byte-for-byte what it was")
	rows := p.dump(t, a)
	var reinforced int
	for _, m := range rows {
		if m.Category == memory.CategoryProfileFact {
			require.NotEqual(t, bFact.ID, m.ID)
			if m.ObservationCount == 2 {
				reinforced++
			}
		}
	}
	require.Equal(t, 1, reinforced, "A's own fact was reinforced")
}

// TestDeleteRepo_DoesNotFollowCrossUserChainPointers: a superseded_by pointer from another user's row
// (the FK allows it) is not part of the chain of A; deleting A's memory leaves that row.
func TestDeleteRepo_DoesNotFollowCrossUserChainPointers(t *testing.T) {
	p := newPGMemory(t)
	a, b := p.user(t), p.user(t)
	am := pgMem(a, memory.CategoryProfileFact, "occupation")
	require.NoError(t, p.insert(t, am))
	bm := pgMem(b, memory.CategoryProfileFact, "occupation", func(m *memory.Memory) { m.Status = memory.StatusSuperseded })
	require.NoError(t, p.insert(t, bm))
	_, err := p.pool.Exec(context.Background(), `UPDATE memories SET superseded_by = $1 WHERE id = $2`, am.ID, bm.ID)
	require.NoError(t, err)

	require.NoError(t, p.store.MemoryStore().WithinTx(context.Background(), func(ctx context.Context, tx memory.Tx) error {
		ok, err := tx.Memories().Delete(ctx, a, am.ID, memAt)
		require.True(t, ok)
		return err
	}))
	rows := p.dump(t, b)
	require.Len(t, rows, 1, "B's row is not in A's chain")
	require.Equal(t, bm.ID, rows[0].ID)
}

func TestMemoryLockUserSerialisesWritersOfOneUser(t *testing.T) {
	p := newPGMemory(t)
	ctx := context.Background()
	u := p.user(t)
	store := p.store.MemoryStore()
	entered, release, second := make(chan struct{}), make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- store.WithinTx(ctx, func(ctx context.Context, tx memory.Tx) error {
			if err := tx.Memories().LockUser(ctx, u); err != nil {
				return err
			}
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered
	go func() {
		_ = store.WithinTx(ctx, func(ctx context.Context, tx memory.Tx) error {
			err := tx.Memories().LockUser(ctx, u)
			close(second)
			return err
		})
	}()
	select {
	case <-second:
		t.Fatal("second writer took the lock while the first holds it")
	case <-time.After(300 * time.Millisecond):
	}
	require.NoError(t, store.WithinTx(ctx, func(ctx context.Context, tx memory.Tx) error {
		return tx.Memories().LockUser(ctx, p.user(t)) // another user is not blocked
	}))
	close(release)
	require.NoError(t, <-done)
	select {
	case <-second:
	case <-time.After(5 * time.Second):
		t.Fatal("second writer never got the lock after release")
	}
}

func TestListExistingForPrompt(t *testing.T) {
	p := newPGMemory(t)
	a, b := p.user(t), p.user(t)
	for _, m := range []memory.Memory{
		pgMem(a, memory.CategoryProfileFact, "occupation", func(m *memory.Memory) { m.LastObservedAt = memAt.Add(-time.Hour) }),
		pgMem(a, memory.CategoryPreference, "hobby"),
		pgMem(a, memory.CategoryEpisodic, "episode:1"),
		pgMem(a, memory.CategoryPreference, "expired", func(m *memory.Memory) { m.ExpiresAt = memAt }),
		pgMem(a, memory.CategoryPreference, "old", func(m *memory.Memory) { m.Status = memory.StatusSuperseded }),
		pgMem(b, memory.CategoryPreference, "other"),
	} {
		require.NoError(t, p.insert(t, m))
	}
	q := sqlcgen.New(p.pool)
	rows, err := q.ListExistingForPrompt(context.Background(), sqlcgen.ListExistingForPromptParams{
		UserID: a, Now: pgtype.Timestamptz{Time: memAt, Valid: true}, MaxRows: 150,
	})
	require.NoError(t, err)
	var keys []string
	for _, r := range rows {
		keys = append(keys, r.Category+"/"+r.MemoryKey)
	}
	require.Equal(t, []string{"PREFERENCE/hobby", "PROFILE_FACT/occupation"}, keys, "own core memories, newest first, no episodes")
	rows, err = q.ListExistingForPrompt(context.Background(), sqlcgen.ListExistingForPromptParams{UserID: a, Now: pgtype.Timestamptz{Time: memAt, Valid: true}, MaxRows: 1})
	require.NoError(t, err)
	require.Len(t, rows, 1)
}
