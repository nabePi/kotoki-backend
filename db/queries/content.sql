-- name: GetTopicRow :one
SELECT * FROM topics WHERE id = $1;

-- name: GetScenarioRow :one
SELECT * FROM scenarios WHERE id = $1;

-- name: GetScenarioVersionRow :one
SELECT * FROM scenario_versions WHERE id = $1;

-- name: GetPublishedScenarioVersionRow :one
SELECT * FROM scenario_versions WHERE scenario_id = $1 AND status = 'PUBLISHED';

-- name: ListScenarioVersionRows :many
SELECT * FROM scenario_versions WHERE scenario_id = $1 ORDER BY version DESC;

-- name: ListPublishedScenarioVersionRows :many
SELECT * FROM scenario_versions WHERE status = 'PUBLISHED' ORDER BY scenario_id;

-- name: ListActiveTopicRows :many
SELECT * FROM topics WHERE status = 'ACTIVE' ORDER BY sort_order, code LIMIT 200;

-- name: ListActiveScenarioRows :many
SELECT * FROM scenarios WHERE topic_id = $1 AND status = 'ACTIVE' ORDER BY sort_order, code LIMIT 200;

-- name: GetCharacterRow :one
SELECT * FROM characters WHERE id = $1;

-- name: ListActiveCharacterRows :many
SELECT * FROM characters WHERE status = 'ACTIVE' ORDER BY sort_order, code LIMIT 200;

-- name: GetPromptVersionRow :one
SELECT pv.*, pc.key AS config_key FROM prompt_versions pv JOIN prompt_configs pc ON pc.id = pv.prompt_config_id WHERE pv.id = $1;

-- name: GetPublishedPromptVersionRow :one
SELECT pv.*, pc.key AS config_key FROM prompt_versions pv JOIN prompt_configs pc ON pc.id = pv.prompt_config_id
 WHERE pc.key = $1 AND pv.status = 'PUBLISHED';

-- name: ListPromptVersionRows :many
SELECT pv.*, pc.key AS config_key FROM prompt_versions pv JOIN prompt_configs pc ON pc.id = pv.prompt_config_id
 WHERE pc.key = $1 ORDER BY pv.version DESC;

-- name: ListPublishedPromptVersionRows :many
SELECT pv.*, pc.key AS config_key FROM prompt_versions pv JOIN prompt_configs pc ON pc.id = pv.prompt_config_id
 WHERE pv.status = 'PUBLISHED' ORDER BY pc.key;

-- Write side (inside a transaction; lock order: parent, then version).

-- name: LockScenarioParent :one
SELECT id FROM scenarios WHERE id = $1 FOR UPDATE;

-- name: LockPromptConfigParent :one
SELECT id FROM prompt_configs WHERE key = $1 FOR UPDATE;

-- name: LockScenarioVersionRow :one
SELECT * FROM scenario_versions WHERE id = $1 AND scenario_id = $2 FOR UPDATE;

-- name: LockPromptVersionRow :one
SELECT pv.*, pc.key AS config_key FROM prompt_versions pv JOIN prompt_configs pc ON pc.id = pv.prompt_config_id
 WHERE pv.id = $1 AND pv.prompt_config_id = $2 FOR UPDATE OF pv;

-- name: NextScenarioVersionNumber :one
SELECT (COALESCE(MAX(version), 0) + 1)::int AS next FROM scenario_versions WHERE scenario_id = $1;

-- name: NextPromptVersionNumber :one
SELECT (COALESCE(MAX(version), 0) + 1)::int AS next FROM prompt_versions WHERE prompt_config_id = $1;

-- name: InsertScenarioVersionDraft :exec
INSERT INTO scenario_versions (id, scenario_id, version, status, difficulty_level, recommended_level_min, recommended_level_max,
    situation, ai_role, user_role, goal, tasks, constraints, grammar_targets, vocab_hints, evaluation_focus, formality,
    opening_line, visual_prompt, notes, created_at)
VALUES ($1, $2, $3, 'DRAFT', $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20);

-- name: InsertPromptVersionDraft :exec
INSERT INTO prompt_versions (id, prompt_config_id, version, status, system_template, user_template, output_schema, llm_params, notes, created_at)
VALUES ($1, $2, $3, 'DRAFT', $4, $5, $6, $7, $8, $9);

-- name: RetirePublishedScenarioVersion :one
UPDATE scenario_versions SET status = 'RETIRED', retired_at = sqlc.arg(at)::timestamptz
 WHERE scenario_id = $1 AND status = 'PUBLISHED' RETURNING id;

-- name: RetirePublishedPromptVersion :one
UPDATE prompt_versions SET status = 'RETIRED', retired_at = sqlc.arg(at)::timestamptz
 WHERE prompt_config_id = $1 AND status = 'PUBLISHED' RETURNING id;

-- name: PublishScenarioVersion :execrows
UPDATE scenario_versions SET status = 'PUBLISHED', published_at = COALESCE(published_at, sqlc.arg(at)::timestamptz), retired_at = NULL WHERE id = $1;

-- name: PublishPromptVersion :execrows
UPDATE prompt_versions SET status = 'PUBLISHED', published_at = COALESCE(published_at, sqlc.arg(at)::timestamptz), retired_at = NULL WHERE id = $1;

-- name: RetireScenarioVersion :execrows
UPDATE scenario_versions SET status = 'RETIRED', retired_at = sqlc.arg(at)::timestamptz WHERE id = $1;

-- name: RetirePromptVersion :execrows
UPDATE prompt_versions SET status = 'RETIRED', retired_at = sqlc.arg(at)::timestamptz WHERE id = $1;

-- name: DeleteScenarioVersion :execrows
DELETE FROM scenario_versions WHERE id = $1;

-- name: DeletePromptVersion :execrows
DELETE FROM prompt_versions WHERE id = $1;

-- name: SetTopicStatus :execrows
UPDATE topics SET status = $2, updated_at = sqlc.arg(at)::timestamptz WHERE id = $1;

-- name: SetScenarioStatus :execrows
UPDATE scenarios SET status = $2, updated_at = sqlc.arg(at)::timestamptz WHERE id = $1;

-- name: LockCharacterRow :one
SELECT * FROM characters WHERE id = $1 FOR UPDATE;

-- name: UpdateCharacterRow :execrows
UPDATE characters SET status = $2, sort_order = $3, name = $4, avatar_key = $5, persona = $6, greeting_hint = $7,
    provider_name = $8, model_name = $9, voice_id = $10, voice = $11, updated_at = sqlc.arg(at)::timestamptz
 WHERE id = $1;

-- spec 6.11: voice exposure of a learner, from the snapshots of COMPLETED sessions.
-- name: GetUserVoiceExposure :many
SELECT (s.config_snapshot->'character'->'voice'->>'gender')::text AS gender,
       (s.config_snapshot->'character'->'voice'->>'accent')::text AS accent,
       COUNT(*)::INTEGER                           AS session_count,
       COALESCE(SUM(s.charged_seconds), 0)::BIGINT AS seconds
FROM sessions s
WHERE s.user_id = $1
  AND s.status = 'COMPLETED'
GROUP BY 1, 2;
