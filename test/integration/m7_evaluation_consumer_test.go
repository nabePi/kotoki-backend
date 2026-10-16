//go:build integration

package integration_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/evaluation"
	"github.com/nabePi/kotoki-backend/internal/events"
	"github.com/nabePi/kotoki-backend/internal/platform/clock"
	"github.com/nabePi/kotoki-backend/internal/platform/idgen"
	"github.com/nabePi/kotoki-backend/internal/postgres"
	"github.com/nabePi/kotoki-backend/internal/provider"
	"github.com/nabePi/kotoki-backend/internal/provider/fake"
	"github.com/nabePi/kotoki-backend/internal/testutil"
)

// scriptedEval is the evaluation provider of these scenarios: step n (0-based) answers call n;
// calls past the script go to fallback (the fake provider) or fail the test.
type scriptedEval struct {
	mu       sync.Mutex
	calls    int
	steps    []func(provider.StructuredRequest) (provider.StructuredResult, error)
	fallback provider.EvaluationProvider
}

func (s *scriptedEval) Name() provider.Name { return provider.FakePrimary }

func (s *scriptedEval) Evaluation() provider.EvaluationProvider { return s }

func (s *scriptedEval) GenerateJSON(ctx context.Context, req provider.StructuredRequest) (provider.StructuredResult, error) {
	s.mu.Lock()
	n := s.calls
	s.calls++
	s.mu.Unlock()
	if n < len(s.steps) {
		return s.steps[n](req)
	}
	if s.fallback != nil {
		return s.fallback.GenerateJSON(ctx, req)
	}
	return provider.StructuredResult{}, provider.NewError(provider.FakePrimary, provider.CapEvaluation, provider.KindInvalidRequest, "scriptedEval: unexpected call")
}

func (s *scriptedEval) Calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func newFakeEval() *fake.Evaluation {
	return fake.NewEvaluation(provider.FakePrimary, fake.CapConfig{}, fake.Config{}, fake.Deps{Seed: 42})
}

// withFake answers every call with the fake provider.
func withFake() *scriptedEval { return &scriptedEval{fallback: newFakeEval()} }

func invalidJSON(provider.StructuredRequest) (provider.StructuredResult, error) {
	return provider.StructuredResult{JSON: json.RawMessage(`{"not":"the rubric"}`), Provider: provider.FakePrimary, Model: "fake"}, nil
}

type evalRig struct {
	pool *pgxpool.Pool
	prov *scriptedEval
	cons *evaluation.Consumer
}

func newEvalRig(t *testing.T, prov *scriptedEval) (*evalRig, *pgxpool.Pool) {
	t.Helper()
	pool := testutil.NewPostgres(t)
	r := &evalRig{pool: pool}
	r.setProvider(prov)
	return r, pool
}

func (r *evalRig) setProvider(prov *scriptedEval) {
	r.prov = prov
	cfg := evaluation.DefaultConfig()
	svc := evaluation.NewService(evaluation.Deps{
		Store: postgres.NewStore(r.pool).EvaluationStore(), Source: postgres.NewEvaluationSource(r.pool),
		Content: postgres.NewContentRepo(r.pool), Router: prov, Cfg: cfg,
		Clock: clock.System{}, IDs: idgen.NewV7(), Schemas: evaluation.NewSchemaCache(), Log: slog.New(slog.DiscardHandler),
	})
	r.cons = evaluation.NewConsumer(svc, cfg)
}

func (r *evalRig) handle(fs testutil.FinishedSession, retry int) (events.Delivery, error) {
	d := events.Delivery{Envelope: fs.Envelope, Queue: events.QueueEvaluation, RetryCount: retry}
	return d, r.cons.Handle(context.Background(), d)
}

var jaTurns = []string{
	"昨日は友達と一緒に映画を見ました。", "今日は会社で会議がありました。", "週末は家族と公園に行きたいです。",
	"毎朝コーヒーを飲んでから出かけます。", "来月は京都へ旅行する予定です。",
}

func seedTurns(n int, seconds float64) []testutil.TurnSeed {
	out := make([]testutil.TurnSeed, n)
	for i := range out {
		out[i] = testutil.TurnSeed{UserText: jaTurns[i%len(jaTurns)], AssistantText: "そうですか。", UserSeconds: seconds}
	}
	return out
}

func count(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) int {
	t.Helper()
	var n int
	require.NoError(t, pool.QueryRow(context.Background(), sql, args...).Scan(&n))
	return n
}

func outboxCount(t *testing.T, pool *pgxpool.Pool, typ string, sessionID uuid.UUID) int {
	t.Helper()
	return count(t, pool, `SELECT count(*) FROM event_outbox o WHERE o.event_type = $1 AND o.payload->>'session_id' = $2`, typ, sessionID.String())
}

func processedCount(t *testing.T, pool *pgxpool.Pool, eventID uuid.UUID) int {
	t.Helper()
	return count(t, pool, `SELECT count(*) FROM processed_events WHERE consumer = 'evaluation.session_completed' AND event_id = $1`, eventID)
}

type evalRow struct {
	Status        string
	Insufficient  bool
	Grammar       *int
	Overall       *float64
	Listening     *int
	Interaction   *int
	Confidence    string
	Comments      string
	LLMCalls      int
	Attempts      int
	FailureCode   *string
	UserTurns     *int
	SpeechSeconds *int
	ActiveSeconds *int
	Provider      *string
}

func loadEval(t *testing.T, pool *pgxpool.Pool, sessionID uuid.UUID) evalRow {
	t.Helper()
	var r evalRow
	require.NoError(t, pool.QueryRow(context.Background(), `
		SELECT status, insufficient_evidence, grammar_score, overall_score::float8, listening_response_signal,
		       interaction_complexity_signal, dimension_confidence::text, dimension_comments::text, llm_call_count,
		       attempt_count, failure_code, user_turn_count, user_speech_seconds, active_seconds, provider
		FROM evaluations WHERE session_id = $1`, sessionID).Scan(
		&r.Status, &r.Insufficient, &r.Grammar, &r.Overall, &r.Listening, &r.Interaction, &r.Confidence, &r.Comments,
		&r.LLMCalls, &r.Attempts, &r.FailureCode, &r.UserTurns, &r.SpeechSeconds, &r.ActiveSeconds, &r.Provider))
	return r
}

func TestEvaluationConsumer_Completed(t *testing.T) {
	rig, pool := newEvalRig(t, withFake())
	fs := testutil.SeedFinishedSession(t, pool, testutil.FinishedSessionOpts{Turns: seedTurns(5, 25), ActiveSeconds: 120})

	_, err := rig.handle(fs, 0)
	require.NoError(t, err)

	e := loadEval(t, pool, fs.ID)
	require.Equal(t, "COMPLETED", e.Status)
	require.False(t, e.Insufficient)
	require.NotNil(t, e.Overall)
	require.NotNil(t, e.Listening)
	require.NotNil(t, e.Interaction)
	require.JSONEq(t, `{"GRAMMAR":0.8,"VOCABULARY":0.7,"FLUENCY":0.75,"NATURALNESS":0.7,"TASK_ACHIEVEMENT":0.8,"LISTENING_RESPONSE":0.7,"INTERACTION_COMPLEXITY":0.6}`, e.Confidence)
	require.NotEqual(t, "{}", e.Comments)
	require.Equal(t, 1, e.LLMCalls)
	require.Equal(t, 1, e.Attempts)
	require.Equal(t, 5, *e.UserTurns)
	require.Equal(t, 125, *e.SpeechSeconds)
	require.Equal(t, 120, *e.ActiveSeconds)
	require.Equal(t, "FAKE_PRIMARY", *e.Provider)

	// the fake derives one finding for every third USER turn; its turn id must resolve to turn 3
	var turnID *uuid.UUID
	var typ string
	rows, err := pool.Query(context.Background(), `SELECT session_turn_id, finding_type FROM evaluation_findings f JOIN evaluations e ON e.id = f.evaluation_id WHERE e.session_id = $1`, fs.ID)
	require.NoError(t, err)
	n := 0
	for rows.Next() {
		require.NoError(t, rows.Scan(&turnID, &typ))
		n++
	}
	rows.Close()
	require.Equal(t, 1, n)
	require.Equal(t, "UNNATURAL_EXPRESSION", typ)
	require.NotNil(t, turnID)
	require.Equal(t, fs.TurnIDs[2], *turnID)

	require.Equal(t, 1, outboxCount(t, pool, "evaluation.completed", fs.ID))
	var aggType, payload string
	require.NoError(t, pool.QueryRow(context.Background(), `SELECT aggregate_type, payload::text FROM event_outbox WHERE event_type = 'evaluation.completed' AND payload->>'session_id' = $1`, fs.ID.String()).Scan(&aggType, &payload))
	require.Equal(t, "EVALUATION", aggType)
	var pl map[string]any
	require.NoError(t, json.Unmarshal([]byte(payload), &pl))
	for _, k := range []string{"scores", "signals", "finding_counts", "overall_score"} {
		require.Contains(t, pl, k)
	}
	require.Equal(t, 1, processedCount(t, pool, fs.Envelope.EventID))
}

func TestEvaluationConsumer_DuplicateSequential(t *testing.T) {
	rig, pool := newEvalRig(t, withFake())
	fs := testutil.SeedFinishedSession(t, pool, testutil.FinishedSessionOpts{Turns: seedTurns(5, 25), ActiveSeconds: 120})

	_, err := rig.handle(fs, 0)
	require.NoError(t, err)
	_, err = rig.handle(fs, 0)
	require.ErrorIs(t, err, events.ErrAlreadyProcessed)

	require.Equal(t, 1, count(t, pool, `SELECT count(*) FROM evaluations WHERE session_id = $1`, fs.ID))
	require.Equal(t, 1, outboxCount(t, pool, "evaluation.completed", fs.ID))
	require.Equal(t, 1, processedCount(t, pool, fs.Envelope.EventID))
	require.Equal(t, 1, rig.prov.Calls(), "the redelivery must not call the provider again")
}

func TestEvaluationConsumer_DuplicateParallel(t *testing.T) {
	rig, pool := newEvalRig(t, withFake())
	fs := testutil.SeedFinishedSession(t, pool, testutil.FinishedSessionOpts{Turns: seedTurns(5, 25), ActiveSeconds: 120})

	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = rig.handle(fs, 0)
		}()
	}
	wg.Wait()

	nilCount := 0
	for _, err := range errs {
		if err == nil {
			nilCount++
			continue
		}
		require.ErrorIs(t, err, events.ErrAlreadyProcessed)
	}
	require.Equal(t, 1, nilCount, "exactly one copy completes: %v", errs)
	require.Equal(t, 1, count(t, pool, `SELECT count(*) FROM evaluations WHERE session_id = $1 AND status = 'COMPLETED'`, fs.ID))
	require.Equal(t, 1, outboxCount(t, pool, "evaluation.completed", fs.ID))
	require.Equal(t, 1, processedCount(t, pool, fs.Envelope.EventID))
}

func TestEvaluationConsumer_RepairOnce(t *testing.T) {
	prov := &scriptedEval{steps: []func(provider.StructuredRequest) (provider.StructuredResult, error){invalidJSON}, fallback: newFakeEval()}
	rig, pool := newEvalRig(t, prov)
	fs := testutil.SeedFinishedSession(t, pool, testutil.FinishedSessionOpts{Turns: seedTurns(5, 25), ActiveSeconds: 120})

	_, err := rig.handle(fs, 0)
	require.NoError(t, err)

	e := loadEval(t, pool, fs.ID)
	require.Equal(t, "COMPLETED", e.Status)
	require.Equal(t, 2, e.LLMCalls)
	require.Equal(t, 2, prov.Calls(), "at most 2 provider calls per delivery")
}

func TestEvaluationConsumer_ProviderErrorNoFallback(t *testing.T) {
	boom := provider.NewError(provider.FakePrimary, provider.CapEvaluation, provider.KindUnavailable, "vendor down")
	prov := &scriptedEval{steps: []func(provider.StructuredRequest) (provider.StructuredResult, error){
		func(provider.StructuredRequest) (provider.StructuredResult, error) {
			return provider.StructuredResult{}, boom
		},
	}}
	rig, pool := newEvalRig(t, prov)
	fs := testutil.SeedFinishedSession(t, pool, testutil.FinishedSessionOpts{Turns: seedTurns(5, 25), ActiveSeconds: 120})

	_, err := rig.handle(fs, 0)
	require.Error(t, err)
	var pe *provider.Error
	require.ErrorAs(t, err, &pe)

	require.Equal(t, 1, prov.Calls(), "no second call after a provider error")
	require.Equal(t, "RUNNING", loadEval(t, pool, fs.ID).Status)
	require.Zero(t, outboxCount(t, pool, "evaluation.completed", fs.ID))
	require.Zero(t, processedCount(t, pool, fs.Envelope.EventID))
}

func TestEvaluationConsumer_InvalidEverywhere(t *testing.T) {
	prov := &scriptedEval{fallback: nil}
	for range 10 {
		prov.steps = append(prov.steps, invalidJSON)
	}
	rig, pool := newEvalRig(t, prov)
	fs := testutil.SeedFinishedSession(t, pool, testutil.FinishedSessionOpts{Turns: seedTurns(5, 25), ActiveSeconds: 120})
	ctx := context.Background()

	for retry := range 5 {
		d, err := rig.handle(fs, retry)
		require.Error(t, err, "attempt %d", retry+1)
		require.ErrorIs(t, err, evaluation.ErrInvalidModelOutput)
		final := retry == 4
		rig.cons.OnFailure(ctx, d, err, final)
		e := loadEval(t, pool, fs.ID)
		if final {
			require.Equal(t, "FAILED", e.Status)
		} else {
			require.Equal(t, "PENDING", e.Status, "attempt %d", retry+1)
		}
	}
	e := loadEval(t, pool, fs.ID)
	require.Equal(t, "FAILED", e.Status)
	require.NotNil(t, e.FailureCode)
	require.Equal(t, "INVALID_MODEL_OUTPUT", *e.FailureCode)
	require.Equal(t, 5, e.Attempts)
	require.Equal(t, 10, prov.Calls())
	require.Equal(t, 1, outboxCount(t, pool, "evaluation.failed", fs.ID))
	require.Zero(t, processedCount(t, pool, fs.Envelope.EventID), "a final failure must stay replayable")

	// DLQ replay with a healthy provider
	rig.setProvider(withFake())
	_, err := rig.handle(fs, 0)
	require.NoError(t, err)
	e = loadEval(t, pool, fs.ID)
	require.Equal(t, "COMPLETED", e.Status)
	require.Equal(t, 6, e.Attempts)
	require.Nil(t, e.FailureCode)
	require.Equal(t, 1, outboxCount(t, pool, "evaluation.completed", fs.ID))
	require.Equal(t, 1, outboxCount(t, pool, "evaluation.failed", fs.ID))
	require.Equal(t, 1, processedCount(t, pool, fs.Envelope.EventID))
}

func TestEvaluationConsumer_DeletedUser(t *testing.T) {
	prov := &scriptedEval{}
	rig, pool := newEvalRig(t, prov)
	fs := testutil.SeedFinishedSession(t, pool, testutil.FinishedSessionOpts{UserStatus: "DELETED", Turns: seedTurns(5, 25), ActiveSeconds: 120})

	_, err := rig.handle(fs, 0)
	require.ErrorIs(t, err, events.ErrSkippedDeletedUser)

	require.Zero(t, count(t, pool, `SELECT count(*) FROM evaluations WHERE session_id = $1`, fs.ID))
	require.Zero(t, outboxCount(t, pool, "evaluation.completed", fs.ID))
	require.Equal(t, 1, processedCount(t, pool, fs.Envelope.EventID))
	require.Zero(t, prov.Calls())
}

func TestEvaluationConsumer_Insufficient(t *testing.T) {
	prov := &scriptedEval{}
	rig, pool := newEvalRig(t, prov)
	fs := testutil.SeedFinishedSession(t, pool, testutil.FinishedSessionOpts{Turns: seedTurns(2, 25), ActiveSeconds: 120})

	_, err := rig.handle(fs, 0)
	require.NoError(t, err)

	e := loadEval(t, pool, fs.ID)
	require.Equal(t, "COMPLETED", e.Status)
	require.True(t, e.Insufficient)
	require.Nil(t, e.Grammar)
	require.Nil(t, e.Overall)
	require.Nil(t, e.Listening)
	require.Zero(t, e.LLMCalls)
	require.Zero(t, prov.Calls())
	require.Zero(t, count(t, pool, `SELECT count(*) FROM evaluation_findings f JOIN evaluations e ON e.id = f.evaluation_id WHERE e.session_id = $1`, fs.ID))
	require.Equal(t, 1, outboxCount(t, pool, "evaluation.completed", fs.ID))
	var insufficient bool
	require.NoError(t, pool.QueryRow(context.Background(), `SELECT (payload->>'insufficient_evidence')::boolean FROM event_outbox WHERE event_type = 'evaluation.completed' AND payload->>'session_id' = $1`, fs.ID.String()).Scan(&insufficient))
	require.True(t, insufficient)
}

func TestEvaluationConsumer_FailedSessionStillEvaluated(t *testing.T) {
	rig, pool := newEvalRig(t, withFake())
	fs := testutil.SeedFinishedSession(t, pool, testutil.FinishedSessionOpts{Status: "FAILED", Turns: seedTurns(5, 25), ActiveSeconds: 120})

	_, err := rig.handle(fs, 0)
	require.NoError(t, err)

	require.Equal(t, "COMPLETED", loadEval(t, pool, fs.ID).Status)
	require.Equal(t, 1, outboxCount(t, pool, "evaluation.completed", fs.ID))
}

func TestEvaluationConsumer_TranscriptUnavailable(t *testing.T) {
	prov := &scriptedEval{}
	rig, pool := newEvalRig(t, prov)
	fs := testutil.SeedFinishedSession(t, pool, testutil.FinishedSessionOpts{ActiveSeconds: 120}).
		WithPayload(t, func(p *events.SessionCompletedV1) { p.UserTurnCount = 3 })

	d, err := rig.handle(fs, 0)
	var perm *events.PermanentError
	require.ErrorAs(t, err, &perm)
	require.Equal(t, "TRANSCRIPT_UNAVAILABLE", perm.Code)

	rig.cons.OnFailure(context.Background(), d, err, true)
	e := loadEval(t, pool, fs.ID)
	require.Equal(t, "FAILED", e.Status)
	require.NotNil(t, e.FailureCode)
	require.Equal(t, "TRANSCRIPT_UNAVAILABLE", *e.FailureCode)
	require.Equal(t, 1, outboxCount(t, pool, "evaluation.failed", fs.ID))
	require.Zero(t, prov.Calls())
}
