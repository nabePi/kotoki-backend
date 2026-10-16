//go:build integration

package integration_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/events"
	"github.com/nabePi/kotoki-backend/internal/memory"
	"github.com/nabePi/kotoki-backend/internal/platform/clock"
	"github.com/nabePi/kotoki-backend/internal/platform/idgen"
	"github.com/nabePi/kotoki-backend/internal/postgres"
	"github.com/nabePi/kotoki-backend/internal/provider"
	"github.com/nabePi/kotoki-backend/internal/provider/fake"
	"github.com/nabePi/kotoki-backend/internal/testutil"
	"github.com/nabePi/kotoki-backend/internal/testutil/fakes"
)

// memProvider is the memory.Provider of these scenarios: GenerateJSON answers with the M4 fake
// evaluation provider (fixture kotoki_memory_extraction_v1), Embed with the M4 fake embedding.
type memProvider struct {
	eval     *fake.Evaluation
	emb      *fake.Embedding
	llmCalls atomic.Int32
	embedErr atomic.Pointer[provider.Error]
}

func newMemProvider() *memProvider {
	return &memProvider{
		eval: fake.NewEvaluation(provider.FakePrimary, fake.CapConfig{}, fake.Config{}, fake.Deps{Seed: 42}),
		emb:  fake.NewEmbedding(provider.FakePrimary, fake.CapConfig{}, fake.Config{}, fake.Deps{Seed: 42}),
	}
}

func (p *memProvider) GenerateJSON(ctx context.Context, req provider.StructuredRequest) (provider.StructuredResult, error) {
	p.llmCalls.Add(1)
	return p.eval.GenerateJSON(ctx, req)
}

func (p *memProvider) Embed(ctx context.Context, req provider.EmbeddingRequest) (provider.EmbeddingResult, error) {
	if err := p.embedErr.Load(); err != nil {
		return provider.EmbeddingResult{}, err
	}
	return p.emb.Embed(ctx, req)
}

type memRig struct {
	t    *testing.T
	pool *pgxpool.Pool
	prov *memProvider
	ver  *fakes.MemVerRecorder
	cons *memory.Consumer
	reg  memory.Reader
}

func newMemRig(t *testing.T) *memRig {
	t.Helper()
	pool := testutil.NewPostgres(t)
	store := postgres.NewStore(pool)
	clk := clock.System{}
	r := &memRig{t: t, pool: pool, prov: newMemProvider(), ver: fakes.NewMemVerRecorder(), reg: store.MemoryStore().Reader()}
	r.cons = memory.NewConsumer(memory.ConsumerDeps{
		Store: store.MemoryStore(), Source: postgres.NewMemorySource(pool, clk),
		Settings: postgres.NewMemoryProfileSettings(pool, idgen.NewV7(), clk), Prompts: postgres.NewMemoryPromptSource(pool),
		Provider: r.prov, MemVer: r.ver, Applier: memory.NewApplier(idgen.NewV7(), clk, nil), Clock: clk, IDs: idgen.NewV7(),
	})
	return r
}

func (r *memRig) session(o testutil.FinishedSessionOpts) testutil.FinishedSession {
	r.t.Helper()
	if o.Turns == nil {
		o.Turns = seedTurns(5, 3)
	}
	if o.ActiveSeconds == 0 {
		o.ActiveSeconds = 120
	}
	return testutil.SeedFinishedSession(r.t, r.pool, o)
}

func (r *memRig) handle(env events.Envelope) error {
	return r.cons.Handle(context.Background(), events.Delivery{Envelope: env, Queue: events.QueueMemory})
}

func (r *memRig) memCount(userID uuid.UUID, where string) int {
	r.t.Helper()
	return count(r.t, r.pool, `SELECT count(*) FROM memories WHERE user_id = $1 `+where, userID)
}

func (r *memRig) processed(eventID uuid.UUID) int {
	r.t.Helper()
	return count(r.t, r.pool, `SELECT count(*) FROM processed_events WHERE consumer = 'memory.session_completed' AND event_id = $1`, eventID)
}

func TestMemoryWorker_ExtractsFromTranscript(t *testing.T) {
	r := newMemRig(t)
	fs := r.session(testutil.FinishedSessionOpts{})
	require.NoError(t, r.handle(fs.Envelope))

	require.Equal(t, 4, r.memCount(fs.UserID, `AND status = 'ACTIVE'`), "3 memories and the episode")
	require.Equal(t, 1, r.memCount(fs.UserID, `AND category = 'EPISODIC'`))
	require.Equal(t, 4, r.memCount(fs.UserID, `AND cardinality(source_turn_ids) >= 1 AND source_session_id = '`+fs.ID.String()+`'`))
	var pv uuid.UUID
	require.NoError(t, r.pool.QueryRow(context.Background(),
		`SELECT pv.id FROM prompt_versions pv JOIN prompt_configs pc ON pc.id = pv.prompt_config_id WHERE pc.key = 'MEMORY_EXTRACTION' AND pv.status = 'PUBLISHED'`).Scan(&pv))
	require.Equal(t, 4, r.memCount(fs.UserID, `AND extraction_prompt_version_id = '`+pv.String()+`'`))
	require.Equal(t, 1, outboxCount(t, r.pool, "memory.extracted", fs.ID))
	require.Equal(t, 1, r.processed(fs.Envelope.EventID))
	require.Equal(t, int64(1), r.ver.Version(fs.UserID))
}

func TestMemoryWorker_RedeliveryDoesNotDoubleCount(t *testing.T) {
	r := newMemRig(t)
	fs := r.session(testutil.FinishedSessionOpts{})
	require.NoError(t, r.handle(fs.Envelope))
	err := r.handle(fs.Envelope)
	require.ErrorIs(t, err, events.ErrAlreadyProcessed)
	require.Equal(t, 0, r.memCount(fs.UserID, `AND observation_count <> 1`))
	require.Equal(t, 4, r.memCount(fs.UserID, ``))
	require.Equal(t, 1, r.processed(fs.Envelope.EventID))
	require.Equal(t, 1, outboxCount(t, r.pool, "memory.extracted", fs.ID))
}

func TestMemoryWorker_SecondSessionReinforces(t *testing.T) {
	r := newMemRig(t)
	first := r.session(testutil.FinishedSessionOpts{})
	require.NoError(t, r.handle(first.Envelope))
	second := r.session(testutil.FinishedSessionOpts{UserID: first.UserID})
	require.NoError(t, r.handle(second.Envelope))

	require.Equal(t, 3, r.memCount(first.UserID, `AND status = 'ACTIVE' AND category <> 'EPISODIC' AND observation_count = 2`))
	require.Equal(t, 4, r.memCount(first.UserID, `AND status = 'ACTIVE'`), "no duplicate slot")
	require.Equal(t, 1, r.memCount(first.UserID, `AND status = 'SUPERSEDED'`), "the old episode is superseded")
	require.Equal(t, int64(2), r.ver.Version(first.UserID))
}

func TestMemoryWorker_UserMismatch(t *testing.T) {
	r := newMemRig(t)
	fs := r.session(testutil.FinishedSessionOpts{})
	other := testutil.SeedUser(t, r.pool, nil)
	env := fs.Envelope
	env.UserID = other // the event claims another user than sessions.user_id
	err := r.handle(env)
	var perm *events.PermanentError
	require.ErrorAs(t, err, &perm)
	require.Equal(t, "INVARIANT_VIOLATION", perm.Code)
	require.Equal(t, 0, r.memCount(fs.UserID, ``))
	require.Equal(t, 0, r.memCount(other, ``))
	require.Equal(t, 0, r.processed(env.EventID))
	require.Equal(t, 0, outboxCount(t, r.pool, "memory.extracted", fs.ID))
	require.Zero(t, r.prov.llmCalls.Load(), "no provider call for a mismatched event")
	require.Empty(t, r.ver.Bumps())
}

func TestMemoryWorker_TombstoneBlocksReextract(t *testing.T) {
	r := newMemRig(t)
	first := r.session(testutil.FinishedSessionOpts{})
	require.NoError(t, r.handle(first.Envelope))
	var id uuid.UUID
	require.NoError(t, r.pool.QueryRow(context.Background(),
		`SELECT id FROM memories WHERE user_id = $1 AND memory_key = 'occupation' AND status = 'ACTIVE'`, first.UserID).Scan(&id))
	store := postgres.NewStore(r.pool).MemoryStore()
	require.NoError(t, store.WithinTx(context.Background(), func(ctx context.Context, tx memory.Tx) error {
		ok, err := tx.Memories().Delete(ctx, first.UserID, id, time.Now().UTC())
		require.True(t, ok)
		return err
	}))

	second := r.session(testutil.FinishedSessionOpts{UserID: first.UserID})
	require.NoError(t, r.handle(second.Envelope))
	require.Equal(t, 0, r.memCount(first.UserID, `AND memory_key = 'occupation' AND status = 'ACTIVE'`), "the deleted slot is not recreated")
	require.Equal(t, 1, r.memCount(first.UserID, `AND memory_key = 'occupation' AND status = 'DELETED'`))
}

func TestMemoryWorker_BumpsMemver(t *testing.T) {
	r := newMemRig(t)
	fs := r.session(testutil.FinishedSessionOpts{})
	require.NoError(t, r.handle(fs.Envelope))
	require.Equal(t, [][]uuid.UUID{{fs.UserID}}, r.ver.Bumps())
}

func TestMemoryWorker_EmbedDownIsRetryableAndWritesNothing(t *testing.T) {
	r := newMemRig(t)
	r.prov.embedErr.Store(&provider.Error{Kind: provider.KindUnavailable, Retryable: true, Message: "embedding down"})
	fs := r.session(testutil.FinishedSessionOpts{})
	err := r.handle(fs.Envelope)
	require.Error(t, err)
	require.True(t, provider.IsRetryable(err))
	require.Equal(t, 0, r.memCount(fs.UserID, ``))
	require.Equal(t, 0, r.processed(fs.Envelope.EventID))
	// the redelivery after the embedding recovered succeeds
	r.prov.embedErr.Store(nil)
	require.NoError(t, r.handle(fs.Envelope))
	require.Equal(t, 4, r.memCount(fs.UserID, `AND status = 'ACTIVE'`))
}

func TestMemoryWorker_DeletedUserAndDisabledMemory(t *testing.T) {
	r := newMemRig(t)
	gone := r.session(testutil.FinishedSessionOpts{UserStatus: "DELETED"})
	require.ErrorIs(t, r.handle(gone.Envelope), events.ErrSkippedDeletedUser)
	require.Equal(t, 1, r.processed(gone.Envelope.EventID))
	require.Zero(t, r.prov.llmCalls.Load())

	off := r.session(testutil.FinishedSessionOpts{})
	_, err := r.pool.Exec(context.Background(), `INSERT INTO learner_profiles (user_id, memory_enabled) VALUES ($1, false)`, off.UserID)
	require.NoError(t, err)
	require.NoError(t, r.handle(off.Envelope))
	require.Equal(t, 1, r.processed(off.Envelope.EventID))
	require.Equal(t, 0, r.memCount(off.UserID, ``))
	require.Zero(t, r.prov.llmCalls.Load())
}

func TestMemoryWorker_TooFewUserTurnsIsEnough(t *testing.T) {
	r := newMemRig(t)
	fs := r.session(testutil.FinishedSessionOpts{Turns: seedTurns(2, 3)})
	require.NoError(t, r.handle(fs.Envelope))
	require.Equal(t, 1, r.processed(fs.Envelope.EventID))
	require.Zero(t, r.prov.llmCalls.Load())
	require.Equal(t, 0, outboxCount(t, r.pool, "memory.extracted", fs.ID))
}
