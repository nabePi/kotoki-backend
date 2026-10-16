//go:build integration

package report_test

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/cmd/kotoki-cli/internal/report"
	"github.com/nabePi/kotoki-backend/internal/testutil"
)

type turnSpec struct {
	session            uuid.UUID
	seq                int
	status             *string
	stt, llm, model    string
	tts, ttsModel      string
	fallback           bool
	e2e                *int
	sttFinal, llmFirst *int
	ttsFirst           *int
	in, out, chars     int
	audioMs            int
	at                 time.Time
}

func sp(s string) *string { return &s }
func ip(i int) *int       { return &i }

// insertTurns writes session_turns without their sessions: the FK is skipped for this
// connection only (session_replication_role), the report never reads the sessions table.
func insertTurns(t *testing.T, pool *pgxpool.Pool, turns []turnSpec) {
	t.Helper()
	ctx := context.Background()
	conn, err := pool.Acquire(ctx)
	require.NoError(t, err)
	defer conn.Release()
	_, err = conn.Exec(ctx, `SET session_replication_role = replica`)
	require.NoError(t, err)
	for _, s := range turns {
		var completed *time.Time
		if s.status != nil {
			completed = &s.at
		}
		_, err := conn.Exec(ctx, `INSERT INTO session_turns (id, session_id, sequence_number, status, generation,
			stt_provider, llm_provider, llm_model, tts_provider, tts_model, used_fallback,
			turn_end_to_first_audio_ms, stt_final_ms, llm_first_token_ms, tts_first_audio_ms,
			input_tokens, output_tokens, tts_characters, stt_audio_ms, created_at, completed_at)
			VALUES ($1,$2,$3,$4,1,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20)`,
			uuid.Must(uuid.NewV7()), s.session, s.seq, s.status, s.stt, s.llm, s.model, s.tts, s.ttsModel, s.fallback,
			s.e2e, s.sttFinal, s.llmFirst, s.ttsFirst, s.in, s.out, s.chars, s.audioMs, s.at, completed)
		require.NoError(t, err)
	}
}

func insertEvaluation(t *testing.T, pool *pgxpool.Pool, session uuid.UUID, status string, scores [5]int, overall float64) {
	t.Helper()
	ctx := context.Background()
	conn, err := pool.Acquire(ctx)
	require.NoError(t, err)
	defer conn.Release()
	_, err = conn.Exec(ctx, `SET session_replication_role = replica`)
	require.NoError(t, err)
	var completed *time.Time
	n := time.Now()
	if status == "COMPLETED" {
		completed = &n
	}
	var code *string
	if status == "FAILED" {
		code = sp("INTERNAL_ERROR")
	}
	_, err = conn.Exec(ctx, `INSERT INTO evaluations (id, session_id, user_id, status, grammar_score, vocabulary_score, fluency_score,
		naturalness_score, task_achievement_score, overall_score, rubric_version, source_event_id, failure_code, completed_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,'v1',$11,$12,$13)`,
		uuid.Must(uuid.NewV7()), session, uuid.Must(uuid.NewV7()), status,
		scores[0], scores[1], scores[2], scores[3], scores[4], overall, uuid.Must(uuid.NewV7()), code, completed)
	require.NoError(t, err)
}

// The range lives inside the current month: session_turns has partitions for this month and the
// next three only.
var (
	monthStart = time.Date(time.Now().UTC().Year(), time.Now().UTC().Month(), 1, 0, 0, 0, 0, time.UTC)
	rFrom      = monthStart.AddDate(0, 0, 2)
	rTo        = monthStart.AddDate(0, 0, 9)
	inDay      = monthStart.AddDate(0, 0, 4).Add(12 * time.Hour)
)

func stackA(session uuid.UUID, seq int, status string, e2e int) turnSpec {
	return turnSpec{session: session, seq: seq, status: sp(status), stt: "OPENAI", llm: "OPENAI", model: "m1", tts: "ELEVENLABS", ttsModel: "t1",
		e2e: ip(e2e), sttFinal: ip(100), llmFirst: ip(200), ttsFirst: ip(300), in: 1000, out: 100, chars: 50, audioMs: 6000, at: inDay}
}

func TestQueryGroupsByStack(t *testing.T) {
	pool := testutil.NewPostgres(t)
	s1, s2, s3 := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())

	var turns []turnSpec
	// stack A: 100..500 ms over 5 completed turns, one FAILED, one INTERRUPTED, one fallback
	for i, v := range []int{100, 200, 300, 400, 500} {
		turns = append(turns, stackA(s1, i+1, "COMPLETED", v))
	}
	failed := stackA(s1, 6, "FAILED", 0)
	failed.e2e, failed.ttsFirst = nil, nil
	turns = append(turns, failed)
	intr := stackA(s1, 7, "INTERRUPTED", 600)
	intr.fallback = true
	turns = append(turns, intr)
	// not counted: DISCARDED, in progress (status NULL), before and at the exclusive end of the range
	disc := stackA(s1, 8, "DISCARDED", 9999)
	turns = append(turns, disc)
	open := stackA(s1, 9, "COMPLETED", 9999)
	open.status = nil
	turns = append(turns, open)
	early := stackA(s1, 10, "COMPLETED", 9999)
	early.at = rFrom.Add(-time.Second)
	turns = append(turns, early)
	atEnd := stackA(s1, 11, "COMPLETED", 9999)
	atEnd.at = rTo
	turns = append(turns, atEnd)
	atStart := stackA(s1, 12, "COMPLETED", 700) // the start is inclusive
	atStart.at = rFrom
	turns = append(turns, atStart)
	// stack B: another LLM model and TTS vendor
	b := stackA(s2, 1, "COMPLETED", 1000)
	b.model, b.tts, b.ttsModel, b.stt = "m2", "CARTESIA", "sonic", "ELEVENLABS"
	turns = append(turns, b)
	// stack C: only a stack differing in the STT vendor; NULL provider columns group as ""
	c := stackA(s3, 1, "COMPLETED", 50)
	c.stt = "CARTESIA"
	turns = append(turns, c)
	insertTurns(t, pool, turns)

	got, err := report.Query(context.Background(), pool, rFrom, rTo)
	require.NoError(t, err)
	require.Len(t, got, 3, "3 stacks; DISCARDED, open and out-of-range turns do not count")
	byKey := map[string]report.TurnAgg{}
	for _, g := range got {
		byKey[g.SttProvider+"/"+g.LLMModel+"/"+g.TTSProvider] = g
	}
	a := byKey["OPENAI/m1/ELEVENLABS"]
	require.Equal(t, 8, a.Turns, "5 completed + FAILED + INTERRUPTED + the turn at the inclusive start")
	require.InDelta(t, 1.0/8, a.FailedRate, 1e-9)
	require.InDelta(t, 1.0/8, a.InterruptedRate, 1e-9)
	require.InDelta(t, 1.0/8, a.TTSFallbackRate, 1e-9)
	// e2e values 100,200,300,400,500,600,700 (the FAILED turn has none): percentile_cont over 7 values
	require.InDelta(t, 400, a.P50TurnEndToFirstAudioMs, 1e-9)
	require.InDelta(t, 100+0.95*6*100, a.P95TurnEndToFirstAudioMs, 1e-9)
	require.InDelta(t, 100+0.99*6*100, a.P99TurnEndToFirstAudioMs, 1e-9)
	require.InDelta(t, 100, a.P95SttFinalMs, 1e-9)
	require.InDelta(t, 200, a.P95LLMFirstTokenMs, 1e-9)
	require.InDelta(t, 300, a.P95TTSFirstAudioMs, 1e-9)
	require.EqualValues(t, 8000, a.InputTokens)
	require.EqualValues(t, 800, a.OutputTokens)
	require.EqualValues(t, 400, a.TTSCharacters)
	require.EqualValues(t, 48000, a.STTAudioMs)
	require.Equal(t, "t1", a.TTSModel)
	require.Equal(t, "OPENAI", a.LLMProvider)

	bb := byKey["ELEVENLABS/m2/CARTESIA"]
	require.Equal(t, 1, bb.Turns)
	require.Equal(t, "sonic", bb.TTSModel)
	require.InDelta(t, 1000, bb.P95TurnEndToFirstAudioMs, 1e-9)
	require.Zero(t, bb.FailedRate)
	require.Equal(t, 1, byKey["CARTESIA/m1/ELEVENLABS"].Turns)
	for _, g := range got {
		require.Nil(t, g.Eval.Overall, "no evaluation rows: empty, not an error")
	}
}

func TestQueryEmptyRangeAndAllNullMetrics(t *testing.T) {
	pool := testutil.NewPostgres(t)
	got, err := report.Query(context.Background(), pool, rFrom, rTo)
	require.NoError(t, err)
	require.Empty(t, got)

	n := stackA(uuid.Must(uuid.NewV7()), 1, "FAILED", 0)
	n.e2e, n.sttFinal, n.llmFirst, n.ttsFirst = nil, nil, nil, nil
	insertTurns(t, pool, []turnSpec{n})
	got, err = report.Query(context.Background(), pool, rFrom, rTo)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.True(t, math.IsNaN(got[0].P95TurnEndToFirstAudioMs), "no value: NaN, not 0")
	require.True(t, math.IsNaN(got[0].P95SttFinalMs))
	require.InDelta(t, 1, got[0].FailedRate, 1e-9)
}

func TestQueryEvalAveragesOnMajorityStack(t *testing.T) {
	pool := testutil.NewPostgres(t)
	ctx := context.Background()
	s1, s2, s3, s4 := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	// s1: 2 turns on stack A, 1 on stack B -> the session counts for A
	b := stackA(s1, 3, "COMPLETED", 100)
	b.model = "m2"
	// s2: a clean A session; s3: only B; s4: A but its evaluation FAILED
	b3 := stackA(s3, 1, "COMPLETED", 100)
	b3.model = "m2"
	insertTurns(t, pool, []turnSpec{
		stackA(s1, 1, "COMPLETED", 100), stackA(s1, 2, "COMPLETED", 100), b,
		stackA(s2, 1, "COMPLETED", 100), b3, stackA(s4, 1, "COMPLETED", 100),
	})
	insertEvaluation(t, pool, s1, "COMPLETED", [5]int{60, 62, 64, 66, 68}, 64)
	insertEvaluation(t, pool, s2, "COMPLETED", [5]int{80, 82, 84, 86, 88}, 84)
	insertEvaluation(t, pool, s3, "COMPLETED", [5]int{10, 20, 30, 40, 50}, 30)
	insertEvaluation(t, pool, s4, "FAILED", [5]int{0, 0, 0, 0, 0}, 0)

	got, err := report.Query(ctx, pool, rFrom, rTo)
	require.NoError(t, err)
	require.Len(t, got, 2)
	by := map[string]report.TurnAgg{}
	for _, g := range got {
		by[g.LLMModel] = g
	}
	a, bb := by["m1"], by["m2"]
	require.NotNil(t, a.Eval.Overall)
	require.InDelta(t, 74, *a.Eval.Overall, 1e-9, "avg of s1 (64) and s2 (84); s1 counts for its majority stack A; the FAILED evaluation is ignored")
	require.InDelta(t, 70, *a.Eval.Grammar, 1e-9)
	require.InDelta(t, 72, *a.Eval.Vocabulary, 1e-9)
	require.InDelta(t, 74, *a.Eval.Fluency, 1e-9)
	require.InDelta(t, 76, *a.Eval.Naturalness, 1e-9)
	require.InDelta(t, 78, *a.Eval.TaskAchievement, 1e-9)
	require.NotNil(t, bb.Eval.Overall)
	require.InDelta(t, 30, *bb.Eval.Overall, 1e-9, "only s3: s1's single stack-B turn does not count for B")
	require.Equal(t, 4, a.Turns, "turns still count every turn of the stack")
}

func TestQueryWithoutEvaluationsTable(t *testing.T) {
	pool := testutil.NewPostgres(t)
	ctx := context.Background()
	insertTurns(t, pool, []turnSpec{stackA(uuid.Must(uuid.NewV7()), 1, "COMPLETED", 100)})
	_, err := pool.Exec(ctx, `ALTER TABLE evaluations RENAME TO evaluations_gone`)
	require.NoError(t, err)
	got, err := report.Query(ctx, pool, rFrom, rTo)
	require.NoError(t, err, "M7 not deployed: avg_eval_* stays empty")
	require.Len(t, got, 1)
	require.Nil(t, got[0].Eval.Overall)
	require.Nil(t, got[0].Eval.Grammar)
}

func TestReportRefusesWrites(t *testing.T) {
	dsn := testutil.NewEmptyPostgresDSN(t)
	ctx := context.Background()
	pool, err := report.OpenReadOnly(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	_, err = pool.Exec(ctx, `CREATE TABLE report_must_not_exist (id int)`)
	require.Error(t, err)
	require.Contains(t, err.Error(), "read-only")
	var v string
	require.NoError(t, pool.QueryRow(ctx, `SHOW default_transaction_read_only`).Scan(&v))
	require.Equal(t, "on", v)
	// a reader still reads
	var one int
	require.NoError(t, pool.QueryRow(ctx, `SELECT 1`).Scan(&one))
}

func TestReportRefusesInsertIntoSessionTurns(t *testing.T) {
	pool := testutil.NewPostgres(t)
	ctx := context.Background()
	ro, err := report.OpenReadOnly(ctx, pool.Config().ConnString())
	require.NoError(t, err)
	t.Cleanup(ro.Close)
	_, err = ro.Exec(ctx, `INSERT INTO session_turns (id, session_id, sequence_number, generation, created_at) VALUES (gen_random_uuid(), gen_random_uuid(), 1, 1, now())`)
	require.Error(t, err)
	require.Contains(t, err.Error(), "read-only")
}

func TestQueryIgnoresInsufficientEvidenceEvaluations(t *testing.T) {
	pool := testutil.NewPostgres(t)
	ctx := context.Background()
	s1, s2 := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	insertTurns(t, pool, []turnSpec{stackA(s1, 1, "COMPLETED", 100), stackA(s2, 1, "COMPLETED", 100)})
	insertEvaluation(t, pool, s1, "COMPLETED", [5]int{60, 60, 60, 60, 60}, 60)
	conn, err := pool.Acquire(ctx)
	require.NoError(t, err)
	defer conn.Release()
	_, err = conn.Exec(ctx, `SET session_replication_role = replica`)
	require.NoError(t, err)
	// no score at all (avg() skips the NULLs, so `NOT insufficient_evidence` is equivalent in the query; kept as intent)
	_, err = conn.Exec(ctx, `INSERT INTO evaluations (id, session_id, user_id, status, insufficient_evidence, rubric_version, source_event_id, completed_at)
		VALUES ($1, $2, $3, 'COMPLETED', true, 'v1', $4, now())`, uuid.Must(uuid.NewV7()), s2, uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()))
	require.NoError(t, err)
	got, err := report.Query(ctx, pool, rFrom, rTo)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.InDelta(t, 60, *got[0].Eval.Overall, 1e-9)
}

func TestQueryNullProviderColumnsFormOneGroupWithEvaluation(t *testing.T) {
	pool := testutil.NewPostgres(t)
	ctx := context.Background()
	s := uuid.Must(uuid.NewV7())
	conn, err := pool.Acquire(ctx)
	require.NoError(t, err)
	defer conn.Release()
	_, err = conn.Exec(ctx, `SET session_replication_role = replica`)
	require.NoError(t, err)
	for i := 1; i <= 2; i++ {
		_, err = conn.Exec(ctx, `INSERT INTO session_turns (id, session_id, sequence_number, status, generation, created_at, completed_at)
			VALUES ($1, $2, $3, 'FAILED', 1, $4, $4)`, uuid.Must(uuid.NewV7()), s, i, inDay)
		require.NoError(t, err)
	}
	insertEvaluation(t, pool, s, "COMPLETED", [5]int{50, 50, 50, 50, 50}, 50)
	got, err := report.Query(ctx, pool, rFrom, rTo)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, 2, got[0].Turns)
	require.Empty(t, got[0].SttProvider)
	require.NotNil(t, got[0].Eval.Overall, "NULL stack columns still join their evaluation")
}
