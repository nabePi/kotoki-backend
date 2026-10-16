package report

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Querier is the part of *pgxpool.Pool the report needs.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// OpenReadOnly opens a pool whose every session has default_transaction_read_only=on: the report
// can never write, whatever the SQL says (spec 8.8.3, read-only).
func OpenReadOnly(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("database url: %w", err)
	}
	if cfg.ConnConfig.RuntimeParams == nil {
		cfg.ConnConfig.RuntimeParams = map[string]string{}
	}
	cfg.ConnConfig.RuntimeParams["default_transaction_read_only"] = "on"
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("database: %w", err)
	}
	return pool, nil
}

// turnsCTE selects the turns of the range. The stack columns are coalesced to ” so that a turn
// without a provider (NULL) still forms a group and joins the evaluation CTE.
const turnsCTE = `
WITH t AS (
  SELECT session_id, status, used_fallback,
         coalesce(stt_provider, '') AS stt_provider, coalesce(llm_provider, '') AS llm_provider,
         coalesce(llm_model, '') AS llm_model, coalesce(tts_provider, '') AS tts_provider,
         coalesce(tts_model, '') AS tts_model,
         turn_end_to_first_audio_ms, stt_final_ms, llm_first_token_ms, tts_first_audio_ms,
         input_tokens, output_tokens, tts_characters, stt_audio_ms
  FROM session_turns
  WHERE created_at >= $1 AND created_at < $2
    AND status IN ('COMPLETED', 'INTERRUPTED', 'FAILED')
),
agg AS (
  SELECT stt_provider, llm_provider, llm_model, tts_provider, tts_model,
         count(*) AS turns,
         percentile_cont(0.50) WITHIN GROUP (ORDER BY turn_end_to_first_audio_ms) AS p50_e2e,
         percentile_cont(0.95) WITHIN GROUP (ORDER BY turn_end_to_first_audio_ms) AS p95_e2e,
         percentile_cont(0.99) WITHIN GROUP (ORDER BY turn_end_to_first_audio_ms) AS p99_e2e,
         percentile_cont(0.95) WITHIN GROUP (ORDER BY stt_final_ms) AS p95_stt_final,
         percentile_cont(0.95) WITHIN GROUP (ORDER BY llm_first_token_ms) AS p95_llm_first_token,
         percentile_cont(0.95) WITHIN GROUP (ORDER BY tts_first_audio_ms) AS p95_tts_first_audio,
         avg((status = 'FAILED')::int)::float8 AS failed_rate,
         avg((status = 'INTERRUPTED')::int)::float8 AS interrupted_rate,
         avg(used_fallback::int)::float8 AS tts_fallback_rate,
         coalesce(sum(input_tokens), 0)::bigint AS input_tokens,
         coalesce(sum(output_tokens), 0)::bigint AS output_tokens,
         coalesce(sum(tts_characters), 0)::bigint AS tts_characters,
         coalesce(sum(stt_audio_ms), 0)::bigint AS stt_audio_ms
  FROM t
  GROUP BY 1, 2, 3, 4, 5
),`

// evalWith averages the completed evaluations per stack. A session with turns on several stacks
// counts for the stack holding most of its turns (ties: the first by name), spec 8.8.3.
const evalWith = `
combo AS (
  SELECT DISTINCT ON (session_id) session_id, stt_provider, llm_provider, llm_model, tts_provider, tts_model
  FROM (
    SELECT session_id, stt_provider, llm_provider, llm_model, tts_provider, tts_model, count(*) AS n
    FROM t GROUP BY 1, 2, 3, 4, 5, 6
  ) c
  ORDER BY session_id, n DESC, stt_provider, llm_provider, llm_model, tts_provider, tts_model
),
ev AS (
  SELECT c.stt_provider, c.llm_provider, c.llm_model, c.tts_provider, c.tts_model,
         avg(e.overall_score)::float8 AS overall, avg(e.grammar_score)::float8 AS grammar,
         avg(e.vocabulary_score)::float8 AS vocabulary, avg(e.fluency_score)::float8 AS fluency,
         avg(e.naturalness_score)::float8 AS naturalness, avg(e.task_achievement_score)::float8 AS task_achievement
  FROM combo c
  JOIN evaluations e ON e.session_id = c.session_id AND e.status = 'COMPLETED' AND NOT e.insufficient_evidence
  GROUP BY 1, 2, 3, 4, 5
)`

// evalNone stands in for evalWith when the evaluations table does not exist (M7 not deployed).
const evalNone = `
ev AS (
  SELECT ''::text AS stt_provider, ''::text AS llm_provider, ''::text AS llm_model, ''::text AS tts_provider,
         ''::text AS tts_model, NULL::float8 AS overall, NULL::float8 AS grammar, NULL::float8 AS vocabulary,
         NULL::float8 AS fluency, NULL::float8 AS naturalness, NULL::float8 AS task_achievement
  WHERE false
)`

const selectTail = `
SELECT a.stt_provider, a.llm_provider, a.llm_model, a.tts_provider, a.tts_model, a.turns,
       a.p50_e2e, a.p95_e2e, a.p99_e2e, a.p95_stt_final, a.p95_llm_first_token, a.p95_tts_first_audio,
       a.failed_rate, a.interrupted_rate, a.tts_fallback_rate,
       a.input_tokens, a.output_tokens, a.tts_characters, a.stt_audio_ms,
       ev.overall, ev.grammar, ev.vocabulary, ev.fluency, ev.naturalness, ev.task_achievement
FROM agg a
LEFT JOIN ev USING (stt_provider, llm_provider, llm_model, tts_provider, tts_model)
ORDER BY a.turns DESC, a.stt_provider, a.llm_provider, a.llm_model, a.tts_provider, a.tts_model`

// Query aggregates the turns created in [from, to) with status COMPLETED, INTERRUPTED or FAILED
// per stack (spec 8.8.3). The evaluation columns are filled only when the evaluations table
// exists; without it (or without a completed evaluation) they stay nil, never an error.
func Query(ctx context.Context, db Querier, from, to time.Time) ([]TurnAgg, error) {
	var hasEval bool
	if err := db.QueryRow(ctx, `SELECT to_regclass('public.evaluations') IS NOT NULL`).Scan(&hasEval); err != nil {
		return nil, fmt.Errorf("report: check evaluations table: %w", err)
	}
	sql := turnsCTE + evalNone + selectTail
	if hasEval {
		sql = turnsCTE + evalWith + selectTail
	}
	rows, err := db.Query(ctx, sql, from, to)
	if err != nil {
		return nil, fmt.Errorf("report: query session_turns: %w", err)
	}
	defer rows.Close()
	var out []TurnAgg
	for rows.Next() {
		var a TurnAgg
		var p50, p95, p99, sttF, llmF, ttsF *float64
		if err := rows.Scan(&a.SttProvider, &a.LLMProvider, &a.LLMModel, &a.TTSProvider, &a.TTSModel, &a.Turns,
			&p50, &p95, &p99, &sttF, &llmF, &ttsF,
			&a.FailedRate, &a.InterruptedRate, &a.TTSFallbackRate,
			&a.InputTokens, &a.OutputTokens, &a.TTSCharacters, &a.STTAudioMs,
			&a.Eval.Overall, &a.Eval.Grammar, &a.Eval.Vocabulary, &a.Eval.Fluency, &a.Eval.Naturalness, &a.Eval.TaskAchievement); err != nil {
			return nil, fmt.Errorf("report: scan: %w", err)
		}
		a.P50TurnEndToFirstAudioMs, a.P95TurnEndToFirstAudioMs, a.P99TurnEndToFirstAudioMs = nan(p50), nan(p95), nan(p99)
		a.P95SttFinalMs, a.P95LLMFirstTokenMs, a.P95TTSFirstAudioMs = nan(sttF), nan(llmF), nan(ttsF)
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("report: rows: %w", err)
	}
	return out, nil
}

func nan(v *float64) float64 {
	if v == nil {
		return math.NaN()
	}
	return *v
}
