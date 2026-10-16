-- Learner profile, skill state and proficiency history (spec Bab 12). Isolation (12.10): every query
-- is scoped by user_id. SELECT, UPDATE and DELETE carry the predicate `user_id = ...`; an INSERT
-- writes the user_id column from the caller's argument. Only the Sys-prefixed query may look
-- across users (the worker's existence probe, never reached from a handler).

-- name: EnsureLearnerProfile :exec
INSERT INTO learner_profiles (user_id, created_at, updated_at)
VALUES (sqlc.arg(user_id), sqlc.arg(at), sqlc.arg(at))
ON CONFLICT (user_id) DO NOTHING;

-- name: EnsureLearnerSkillState :exec
INSERT INTO learner_skill_states (id, user_id, skill, prior_level, prior_confidence, estimated_level, confidence,
                                  created_at, updated_at)
VALUES (sqlc.arg(id), sqlc.arg(user_id), sqlc.arg(skill), sqlc.arg(prior_level)::numeric,
        sqlc.arg(prior_confidence)::numeric, sqlc.arg(prior_level)::numeric, sqlc.arg(prior_confidence)::numeric,
        sqlc.arg(at), sqlc.arg(at))
ON CONFLICT (user_id, skill) DO NOTHING;

-- name: GetLearnerProfile :one
SELECT * FROM learner_profiles WHERE user_id = sqlc.arg(user_id);

-- name: UpdateLearnerProfile :one
UPDATE learner_profiles SET
       nickname = sqlc.narg(nickname)::text,
       self_declared_level = sqlc.narg(self_declared_level)::text,
       target_level = sqlc.narg(target_level)::text,
       learning_goal = sqlc.narg(learning_goal)::text,
       learning_goal_note = sqlc.narg(learning_goal_note)::text,
       preferred_speech_speed = sqlc.arg(preferred_speech_speed)::text,
       preferred_formality = sqlc.arg(preferred_formality)::text,
       dialect_preference = sqlc.arg(dialect_preference)::text,
       memory_enabled = sqlc.arg(memory_enabled)::boolean,
       onboarding_completed_at = sqlc.narg(onboarding_completed_at)::timestamptz,
       updated_at = sqlc.arg(at)
 WHERE user_id = sqlc.arg(user_id)
RETURNING *;

-- Serialises every learner write of one user (spec 12.5.3).
-- name: LockLearnerUser :exec
SELECT pg_advisory_xact_lock(hashtextextended('learner:' || sqlc.arg(user_id)::text, 0));

-- name: UpdateLearnerSkillPrior :execrows
UPDATE learner_skill_states SET
       prior_level = sqlc.arg(prior_level)::numeric, prior_confidence = sqlc.arg(prior_confidence)::numeric,
       estimated_level = sqlc.arg(prior_level)::numeric, confidence = sqlc.arg(prior_confidence)::numeric,
       updated_at = sqlc.arg(at)
 WHERE user_id = sqlc.arg(user_id) AND skill = sqlc.arg(skill) AND evidence_count = 0;

-- name: GetLearnerStats :one
SELECT count(*)::int AS completed_sessions,
       COALESCE(sum(charged_seconds), 0)::bigint AS practice_seconds,
       max(ended_at)::timestamptz AS last_session_at
  FROM sessions
 WHERE user_id = sqlc.arg(user_id) AND status = 'COMPLETED';

-- name: ListLearnerSkillStates :many
SELECT * FROM learner_skill_states
 WHERE user_id = sqlc.arg(user_id)
 ORDER BY array_position(ARRAY['GRAMMAR', 'VOCABULARY', 'FLUENCY', 'NATURALNESS', 'LISTENING_RESPONSE',
                               'INTERACTION_COMPLEXITY']::text[], skill);

-- name: ListLearnerSkillStatesForUpdate :many
SELECT * FROM learner_skill_states
 WHERE user_id = sqlc.arg(user_id)
 ORDER BY array_position(ARRAY['GRAMMAR', 'VOCABULARY', 'FLUENCY', 'NATURALNESS', 'LISTENING_RESPONSE',
                               'INTERACTION_COMPLEXITY']::text[], skill)
   FOR UPDATE;

-- name: SaveLearnerSkillState :execrows
UPDATE learner_skill_states SET
       estimated_level = sqlc.arg(estimated_level)::numeric,
       demonstrated_ceiling = sqlc.narg(demonstrated_ceiling)::numeric,
       confidence = sqlc.arg(confidence)::numeric,
       trend = sqlc.arg(trend)::text,
       evidence_count = sqlc.arg(evidence_count)::int,
       last_evaluation_id = sqlc.narg(last_evaluation_id)::uuid,
       last_evidence_at = sqlc.narg(last_evidence_at)::timestamptz,
       updated_at = sqlc.arg(at)
 WHERE user_id = sqlc.arg(user_id) AND skill = sqlc.arg(skill);

-- Spec 12.9.5: back to the priors, no evidence.
-- name: ResetLearnerSkillStates :exec
UPDATE learner_skill_states SET
       estimated_level = prior_level, confidence = prior_confidence, demonstrated_ceiling = NULL,
       trend = 'INSUFFICIENT_DATA', evidence_count = 0, last_evaluation_id = NULL, last_evidence_at = NULL,
       updated_at = sqlc.arg(at)
 WHERE user_id = sqlc.arg(user_id);

-- name: InsertProficiencyEstimate :execrows
INSERT INTO proficiency_estimates (id, user_id, skill, source_evaluation_id, session_id, algorithm_version, raw_score,
                                   effective_difficulty, evidence_level, evidence_confidence, previous_level,
                                   estimated_level, confidence, evidence_count, trend, assessed_at, created_at)
VALUES (sqlc.arg(id), sqlc.arg(user_id), sqlc.arg(skill), sqlc.arg(source_evaluation_id), sqlc.arg(session_id),
        sqlc.arg(algorithm_version)::smallint, sqlc.arg(raw_score)::numeric, sqlc.narg(effective_difficulty)::smallint,
        sqlc.arg(evidence_level)::numeric, sqlc.arg(evidence_confidence)::numeric, sqlc.arg(previous_level)::numeric,
        sqlc.arg(estimated_level)::numeric, sqlc.arg(confidence)::numeric, sqlc.arg(evidence_count)::int,
        sqlc.arg(trend), sqlc.arg(assessed_at), sqlc.arg(at))
ON CONFLICT (source_evaluation_id, skill) DO NOTHING;

-- Replay order of spec 12.5.5.
-- name: ListProficiencyEstimatesBySkill :many
SELECT * FROM proficiency_estimates
 WHERE user_id = sqlc.arg(user_id) AND skill = sqlc.arg(skill)
 ORDER BY assessed_at ASC, source_evaluation_id ASC;

-- Only the derived columns (spec 12.2.3); the evidence columns never change.
-- name: UpdateProficiencyEstimateDerived :execrows
UPDATE proficiency_estimates SET
       previous_level = sqlc.arg(previous_level)::numeric, estimated_level = sqlc.arg(estimated_level)::numeric,
       confidence = sqlc.arg(confidence)::numeric, evidence_count = sqlc.arg(evidence_count)::int,
       trend = sqlc.arg(trend)::text
 WHERE id = sqlc.arg(id) AND user_id = sqlc.arg(user_id);

-- name: DeleteProficiencyEstimates :exec
DELETE FROM proficiency_estimates WHERE user_id = sqlc.arg(user_id);

-- name: ListProficiencyHistory :many
SELECT * FROM proficiency_estimates
 WHERE user_id = sqlc.arg(user_id) AND skill = sqlc.arg(skill)
   AND (sqlc.narg(after_at)::timestamptz IS NULL
        OR (assessed_at, id) < (sqlc.narg(after_at)::timestamptz, sqlc.narg(after_id)::uuid))
 ORDER BY assessed_at DESC, id DESC
 LIMIT sqlc.arg(row_limit)::int;

-- The evidence of one evaluation (spec 12.4): the evaluation must belong to the user and so must
-- its session. n_turns counts the valid turns of 12.4.
-- name: GetLearnerEvidence :one
SELECT e.id, e.session_id, e.user_id, e.status,
       e.grammar_score, e.vocabulary_score, e.fluency_score, e.naturalness_score,
       e.listening_response_signal, e.interaction_complexity_signal, e.dimension_confidence,
       s.ended_at AS session_ended_at, s.adaptive_difficulty, sv.difficulty_level AS scenario_difficulty,
       (SELECT count(*) FROM session_turns t
         WHERE t.session_id = e.session_id
           AND t.created_at >= COALESCE(s.starting_at, s.created_at) - interval '1 minute'
           AND t.status IN ('COMPLETED', 'INTERRUPTED') AND char_length(t.user_text) >= 5)::int AS n_turns
  FROM evaluations e
  JOIN sessions s ON s.id = e.session_id AND s.user_id = e.user_id
  JOIN scenario_versions sv ON sv.id = s.scenario_version_id
 WHERE e.id = sqlc.arg(evaluation_id) AND e.user_id = sqlc.arg(user_id);

-- Tells "not committed yet" from "belongs to somebody else" after GetLearnerEvidence found nothing.
-- name: SysLearnerEvaluationExists :one
SELECT EXISTS (SELECT 1 FROM evaluations WHERE id = sqlc.arg(evaluation_id));

-- name: CountRecentFindingsBySkill :many
WITH recent AS (
    SELECT id FROM evaluations
     WHERE user_id = sqlc.arg(user_id) AND status = 'COMPLETED'
     ORDER BY completed_at DESC, id DESC LIMIT 5
)
SELECT f.dimension,
       count(*) FILTER (WHERE f.severity = 'MAJOR') AS major,
       count(*) FILTER (WHERE f.severity = 'MINOR') AS minor,
       (SELECT count(*) FROM recent) AS evaluations
  FROM evaluation_findings f
  JOIN recent r ON r.id = f.evaluation_id
 WHERE f.user_id = sqlc.arg(user_id)
 GROUP BY f.dimension;

-- name: CountRecentLearnerEvaluations :one
SELECT count(*)::int FROM (
    SELECT id FROM evaluations
     WHERE user_id = sqlc.arg(user_id) AND status = 'COMPLETED'
     ORDER BY completed_at DESC, id DESC LIMIT 5
) recent;

-- name: GetVoiceExposure :many
SELECT COALESCE(s.config_snapshot->'character'->'voice'->>'gender', '')::text AS gender,
       COALESCE(s.config_snapshot->'character'->'voice'->>'accent', '')::text AS accent,
       count(*)::int AS sessions, COALESCE(sum(s.charged_seconds), 0)::bigint AS seconds
  FROM sessions s
 WHERE s.user_id = sqlc.arg(user_id) AND s.status = 'COMPLETED'
   AND s.ended_at > sqlc.arg(now)::timestamptz - interval '90 days'
 GROUP BY 1, 2;

-- name: CountTopicSessions90d :one
SELECT count(*)::int FROM sessions
 WHERE user_id = sqlc.arg(user_id) AND status = 'COMPLETED' AND topic_id = sqlc.arg(topic_id)
   AND ended_at > sqlc.arg(now)::timestamptz - interval '90 days';

-- name: CountDialectSessions90d :one
SELECT count(*)::int FROM sessions
 WHERE user_id = sqlc.arg(user_id) AND status = 'COMPLETED'
   AND config_snapshot->'character'->'voice'->>'accent' = 'KANSAI'
   AND ended_at > sqlc.arg(now)::timestamptz - interval '90 days';
