//go:build integration

package integration_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/memory"
	"github.com/nabePi/kotoki-backend/internal/platform/clock"
	"github.com/nabePi/kotoki-backend/internal/platform/idgen"
	"github.com/nabePi/kotoki-backend/internal/postgres"
	"github.com/nabePi/kotoki-backend/internal/provider"
	"github.com/nabePi/kotoki-backend/internal/redisstore"
	"github.com/nabePi/kotoki-backend/internal/testutil"
)

type retrievalRig struct {
	t     *testing.T
	rig   *memRig
	ret   *memory.Retriever
	bump  *redisstore.MemVerBumper
	cache *redisstore.MemCtxStore
	store memory.Store
}

func newRetrievalRigPG(t *testing.T) (*retrievalRig, *memRig) {
	t.Helper()
	r := newMemRig(t)
	rdb := testutil.NewRedis(t)
	pgStore := postgres.NewStore(r.pool)
	rr := &retrievalRig{t: t, rig: r, store: pgStore.MemoryStore(), cache: redisstore.NewMemCtxStore(rdb), bump: redisstore.NewMemVerBumper(rdb)}
	rr.ret = memory.NewRetriever(memory.RetrieverDeps{
		Reader: pgStore.MemoryStore().Reader(), Settings: postgres.NewMemoryProfileSettings(r.pool, idgen.NewV7(), clock.System{}),
		Embedder: r.prov, Cache: rr.cache, Clock: clock.System{},
	})
	return rr, r
}

// extract runs the worker for a fresh finished session of userID (zero: a new user).
func (rr *retrievalRig) extract(userID uuid.UUID) testutil.FinishedSession {
	rr.t.Helper()
	fs := rr.rig.session(testutil.FinishedSessionOpts{UserID: userID})
	require.NoError(rr.t, rr.rig.handle(fs.Envelope))
	return fs
}

func (rr *retrievalRig) build(userID uuid.UUID, text string) (memory.MemoryContext, uuid.UUID) {
	rr.t.Helper()
	sid := uuid.Must(uuid.NewV7())
	mc, err := rr.ret.BuildSessionContext(context.Background(), memory.SessionContextInput{UserID: userID, SessionID: sid, ScenarioText: text})
	require.NoError(rr.t, err)
	return mc, sid
}

func TestRetrieval_E2EOnPostgres(t *testing.T) {
	rr, _ := newRetrievalRigPG(t)
	first := rr.extract(uuid.Nil)
	require.NoError(t, rr.bump.Bump(context.Background(), []uuid.UUID{first.UserID}))

	// a new session: the episode of the old session is recalled, an episode of the new one never
	mc, sid := rr.build(first.UserID, "面接練習（バックエンドエンジニア）。Goを選んだ理由とマイクロサービスの経験を説明した。")
	require.False(t, mc.Degraded)
	require.NotEmpty(t, mc.Core)
	var keys []string
	for _, it := range mc.Core {
		require.NotEqual(t, memory.CategoryEpisodic, it.Category)
		keys = append(keys, it.Content)
	}
	require.Contains(t, keys, "バックエンドエンジニアとして働いている。")
	require.Len(t, mc.Episodic, 1)
	require.Equal(t, memory.CategoryEpisodic, mc.Episodic[0].Category)
	require.GreaterOrEqual(t, mc.Episodic[0].Score, 0.75)
	require.Equal(t, int64(1), mc.UserMemVer)
	cached, err := rr.cache.GetContext(context.Background(), sid)
	require.NoError(t, err)
	require.NotNil(t, cached, "memctx is written")
	require.Equal(t, mc.Core, cached.Core)

	// the session that produced the episode does not recall its own episode
	own := first.ID
	items, err := rr.ret.RefreshEpisodic(context.Background(), first.UserID, own, []memory.TurnText{
		{UserText: "面接練習（バックエンドエンジニア）。Goを選んだ理由とマイクロサービスの経験を説明した。"}})
	require.NoError(t, err)
	require.Empty(t, items, "the episode of the session itself is excluded")
}

func TestRetrieval_UserIsolation(t *testing.T) {
	rr, _ := newRetrievalRigPG(t)
	a := rr.extract(uuid.Nil)
	b := rr.extract(uuid.Nil) // identical transcripts, therefore identical embeddings
	text := "面接練習（バックエンドエンジニア）。Goを選んだ理由とマイクロサービスの経験を説明した。"

	mcA, _ := rr.build(a.UserID, text)
	mcB, _ := rr.build(b.UserID, text)
	require.NotEmpty(t, mcA.Core)
	ids := map[uuid.UUID]bool{}
	for _, it := range append(append([]memory.ContextItem{}, mcA.Core...), mcA.Episodic...) {
		ids[it.ID] = true
	}
	for _, it := range append(append([]memory.ContextItem{}, mcB.Core...), mcB.Episodic...) {
		require.False(t, ids[it.ID], "no memory of user A reaches user B")
	}
	require.Equal(t, 4, rr.rig.memCount(b.UserID, `AND status = 'ACTIVE'`))

	stranger := testutil.SeedUser(t, rr.rig.pool, nil)
	mcC, _ := rr.build(stranger, text)
	require.Empty(t, mcC.Core)
	require.Empty(t, mcC.Episodic)
	require.False(t, mcC.Degraded)
}

func TestRetrieval_AfterUserDeleteAll_NextReloadEmpty(t *testing.T) {
	rr, _ := newRetrievalRigPG(t)
	fs := rr.extract(uuid.Nil)
	mc, sid := rr.build(fs.UserID, "面接練習")
	require.NotEmpty(t, mc.Core)

	changed, err := rr.ret.ReloadCoreIfChanged(context.Background(), &mc)
	require.NoError(t, err)
	require.False(t, changed, "nothing happened yet")

	require.NoError(t, rr.store.WithinTx(context.Background(), func(ctx context.Context, tx memory.Tx) error {
		return tx.Memories().DeleteAll(ctx, fs.UserID)
	}))
	require.NoError(t, rr.bump.Bump(context.Background(), []uuid.UUID{fs.UserID}))

	changed, err = rr.ret.ReloadCoreIfChanged(context.Background(), &mc)
	require.NoError(t, err)
	require.True(t, changed)
	require.Empty(t, mc.Core)
	require.Empty(t, mc.Episodic)
	require.Zero(t, mc.CoreTokens)
	cached, err := rr.cache.GetContext(context.Background(), sid)
	require.NoError(t, err)
	require.Empty(t, cached.Core, "memctx follows the reload")
}

func TestRetrieval_MemoryDisabledReturnsNothing(t *testing.T) {
	rr, r := newRetrievalRigPG(t)
	fs := rr.extract(uuid.Nil)
	_, err := r.pool.Exec(context.Background(), `UPDATE learner_profiles SET memory_enabled = false WHERE user_id = $1`, fs.UserID)
	require.NoError(t, err)
	mc, sid := rr.build(fs.UserID, "面接練習")
	require.Empty(t, mc.Core)
	require.False(t, mc.Degraded)
	cached, err := rr.cache.GetContext(context.Background(), sid)
	require.NoError(t, err)
	require.Nil(t, cached)
}

func TestRetrieval_EmbeddingDownStillLoadsCore(t *testing.T) {
	rr, _ := newRetrievalRigPG(t)
	fs := rr.extract(uuid.Nil)
	rr.rig.prov.embedErr.Store(&provider.Error{Kind: provider.KindUnavailable, Retryable: true, Message: "down"})
	mc, _ := rr.build(fs.UserID, "面接練習")
	require.True(t, mc.Degraded)
	require.NotEmpty(t, mc.Core)
	require.Empty(t, mc.Episodic)
}
