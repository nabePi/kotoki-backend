-- Post-session evaluation (spec Bab 10). Claim is the upsert of 10.5 step 4: a COMPLETED row is never
-- touched, so no row comes back and the caller acknowledges the delivery.

-- name: ClaimEvaluation :one
INSERT INTO evaluations (id, session_id, user_id, status, rubric_version, prompt_version_id,
                         attempt_count, source_event_id, started_at, created_at, updated_at)
VALUES (sqlc.arg(id), sqlc.arg(session_id), sqlc.arg(user_id), 'RUNNING', sqlc.arg(rubric_version), sqlc.arg(prompt_version_id),
        1, sqlc.arg(source_event_id), sqlc.arg(at), sqlc.arg(at), sqlc.arg(at))
ON CONFLICT (session_id) DO UPDATE
   SET status = 'RUNNING', attempt_count = evaluations.attempt_count + 1,
       started_at = EXCLUDED.started_at, failure_code = NULL, last_error = NULL,
       rubric_version = EXCLUDED.rubric_version, prompt_version_id = EXCLUDED.prompt_version_id,
       updated_at = EXCLUDED.updated_at
 WHERE evaluations.status <> 'COMPLETED'
RETURNING id, attempt_count;

-- name: CompleteEvaluation :execrows
UPDATE evaluations SET
       status = 'COMPLETED',
       insufficient_evidence = sqlc.arg(insufficient_evidence),
       grammar_score = sqlc.narg(grammar_score)::smallint,
       vocabulary_score = sqlc.narg(vocabulary_score)::smallint,
       fluency_score = sqlc.narg(fluency_score)::smallint,
       naturalness_score = sqlc.narg(naturalness_score)::smallint,
       task_achievement_score = sqlc.narg(task_achievement_score)::smallint,
       overall_score = sqlc.narg(overall_score)::numeric,
       listening_response_signal = sqlc.narg(listening_response_signal)::smallint,
       interaction_complexity_signal = sqlc.narg(interaction_complexity_signal)::smallint,
       dimension_confidence = sqlc.arg(dimension_confidence)::jsonb,
       dimension_comments = sqlc.arg(dimension_comments)::jsonb,
       summary_feedback = sqlc.arg(summary_feedback)::text,
       strengths = sqlc.arg(strengths)::jsonb,
       focus_areas = sqlc.arg(focus_areas)::jsonb,
       recommendations = sqlc.arg(recommendations)::jsonb,
       user_turn_count = sqlc.arg(user_turn_count)::int,
       user_speech_seconds = sqlc.arg(user_speech_seconds)::int,
       active_seconds = sqlc.arg(active_seconds)::int,
       scenario_difficulty_level = sqlc.arg(scenario_difficulty_level)::smallint,
       provider = sqlc.narg(provider)::text,
       model = sqlc.narg(model)::text,
       llm_call_count = sqlc.arg(llm_call_count)::smallint,
       input_tokens = sqlc.narg(input_tokens)::int,
       output_tokens = sqlc.narg(output_tokens)::int,
       failure_code = NULL, last_error = NULL,
       completed_at = sqlc.arg(at), updated_at = sqlc.arg(at)
 WHERE id = sqlc.arg(id) AND status <> 'COMPLETED';

-- name: DeleteEvaluationFindings :exec
DELETE FROM evaluation_findings WHERE evaluation_id = $1;

-- name: InsertEvaluationFinding :exec
INSERT INTO evaluation_findings (id, evaluation_id, session_id, user_id, session_turn_id, turn_sequence_number, ordinal,
                                 dimension, finding_type, severity, original_text, corrected_text, explanation,
                                 pattern_key, confidence, source, created_at)
VALUES (sqlc.arg(id), sqlc.arg(evaluation_id), sqlc.arg(session_id), sqlc.arg(user_id), sqlc.narg(session_turn_id),
        sqlc.narg(turn_sequence_number)::int, sqlc.arg(ordinal)::smallint, sqlc.arg(dimension), sqlc.arg(finding_type),
        sqlc.arg(severity), sqlc.narg(original_text), sqlc.narg(corrected_text), sqlc.arg(explanation),
        sqlc.narg(pattern_key), sqlc.arg(confidence)::numeric, sqlc.arg(source), sqlc.arg(created_at));

-- name: MarkEvaluationPending :execrows
UPDATE evaluations SET status = 'PENDING', last_error = left(sqlc.arg(last_error)::text, 2000), updated_at = sqlc.arg(at)
 WHERE session_id = sqlc.arg(session_id) AND status = 'RUNNING';

-- name: MarkEvaluationFailed :one
UPDATE evaluations SET status = 'FAILED', failure_code = sqlc.arg(failure_code)::text,
       last_error = left(sqlc.arg(last_error)::text, 2000), updated_at = sqlc.arg(at)
 WHERE session_id = sqlc.arg(session_id) AND status IN ('RUNNING', 'PENDING')
RETURNING *;

-- name: GetEvaluationBySession :one
SELECT * FROM evaluations WHERE session_id = $1;

-- name: ListEvaluationFindings :many
SELECT * FROM evaluation_findings WHERE evaluation_id = $1 ORDER BY ordinal;

-- name: GetSessionForEvaluation :one
SELECT s.id, s.user_id, s.scenario_id, s.scenario_version_id, s.topic_id, s.status,
       s.starting_at, s.started_at, s.ended_at, s.disconnected_seconds,
       s.adaptive_difficulty, s.config_snapshot, s.transcript_purged_at, u.status AS user_status
  FROM sessions s JOIN users u ON u.id = s.user_id
 WHERE s.id = $1;

-- name: ListTurnsForEvaluation :many
SELECT id, created_at, sequence_number, status, user_text, assistant_text, assistant_spoken_text,
       user_started_at, user_ended_at
  FROM session_turns
 WHERE session_id = sqlc.arg(session_id) AND status IS DISTINCT FROM 'DISCARDED'
   AND created_at >= sqlc.arg(created_from)::timestamptz AND created_at <= sqlc.arg(created_to)::timestamptz
 ORDER BY sequence_number;
