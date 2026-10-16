-- Long-term memory (spec Bab 11). Isolation (11.10): every query is scoped by user_id. SELECT, UPDATE
-- and DELETE carry the predicate `user_id = ...` for each reference to memories (the vector CTEs
-- filter inside the CTE, before ranking); an INSERT writes the user_id column from the caller's
-- argument. Time is a parameter (`now`) so tests are deterministic. The Sys-prefixed cleanup query
-- (SysDeleteExpiredMemories, retention.sql) is the only one that may look across users.

-- name: ListCoreCandidates :many
SELECT id, category, memory_key, content, confidence, importance, observation_count,
       last_observed_at, embedding
  FROM memories
 WHERE user_id = sqlc.arg(user_id)
   AND status = 'ACTIVE'
   AND category IN ('PROFILE_FACT', 'PREFERENCE', 'LEARNING_PATTERN')
   AND expires_at > sqlc.arg(now)::timestamptz
 ORDER BY last_observed_at DESC, id
 LIMIT 200;

-- Exact scan of the user's episodes: user_id filters inside the MATERIALIZED CTE, before ranking, so
-- the planner can never rank across users (spec 11.10 rule 3). A NULL exclude_session_id excludes
-- nothing (a bare IS DISTINCT FROM NULL would drop episodes whose session was deleted).
-- name: SearchEpisodic :many
WITH cand AS MATERIALIZED (
    SELECT id, content, last_observed_at, embedding
      FROM memories
     WHERE user_id = sqlc.arg(user_id)
       AND status = 'ACTIVE'
       AND category = 'EPISODIC'
       AND expires_at > sqlc.arg(now)::timestamptz
       AND embedding IS NOT NULL
       AND (sqlc.narg(exclude_session_id)::uuid IS NULL
            OR source_session_id IS DISTINCT FROM sqlc.narg(exclude_session_id)::uuid)
)
SELECT id, content, last_observed_at,
       (1 - (embedding <=> sqlc.arg(query)::vector))::float8 AS similarity
  FROM cand
 WHERE (1 - (embedding <=> sqlc.arg(query)::vector)) >= sqlc.arg(min_similarity)::float8
 ORDER BY embedding <=> sqlc.arg(query)::vector, id
 LIMIT sqlc.arg(k)::int;

-- name: FindNearestActive :one
WITH cand AS MATERIALIZED (
    SELECT id, memory_key, embedding
      FROM memories
     WHERE user_id = sqlc.arg(user_id) AND status = 'ACTIVE' AND category = sqlc.arg(category)
       AND embedding IS NOT NULL
)
SELECT id, memory_key, (1 - (embedding <=> sqlc.arg(query)::vector))::float8 AS similarity
  FROM cand
 ORDER BY embedding <=> sqlc.arg(query)::vector, id
 LIMIT 1;

-- name: GetActiveSlotForUpdate :one
SELECT * FROM memories
 WHERE user_id = sqlc.arg(user_id) AND category = sqlc.arg(category) AND memory_key = sqlc.arg(memory_key)
   AND status = 'ACTIVE'
 FOR UPDATE;

-- Serialises every memory write of one user (spec 11.6).
-- name: LockMemoryUser :exec
SELECT pg_advisory_xact_lock(hashtextextended('memory:' || sqlc.arg(user_id)::text, 0));

-- name: InsertMemory :exec
INSERT INTO memories (id, user_id, category, memory_key, content, display_text, evidence_type,
                      confidence, importance, observation_count, source_session_id, source_turn_ids,
                      extraction_prompt_version_id, embedding, embedding_model, status, superseded_by,
                      first_observed_at, last_observed_at, expires_at, created_at, updated_at)
VALUES (sqlc.arg(id), sqlc.arg(user_id), sqlc.arg(category), sqlc.arg(memory_key), sqlc.arg(content),
        sqlc.arg(display_text), sqlc.arg(evidence_type), sqlc.arg(confidence)::numeric,
        sqlc.arg(importance)::numeric, sqlc.arg(observation_count), sqlc.narg(source_session_id),
        sqlc.arg(source_turn_ids)::uuid[], sqlc.narg(extraction_prompt_version_id),
        sqlc.narg(embedding)::vector, sqlc.narg(embedding_model)::text, sqlc.arg(status),
        sqlc.narg(superseded_by), sqlc.arg(first_observed_at), sqlc.arg(last_observed_at),
        sqlc.arg(expires_at), sqlc.arg(created_at), sqlc.arg(updated_at));

-- REINFORCE (spec 11.6): confidence = LEAST(0.990, GREATEST(c, c_new) + 0.050); content and
-- embedding are never touched.
-- name: ReinforceMemory :execrows
UPDATE memories SET
       observation_count = observation_count + 1,
       confidence = LEAST(0.990, GREATEST(confidence, sqlc.arg(confidence)::numeric) + 0.050),
       importance = GREATEST(importance, sqlc.arg(importance)::numeric),
       last_observed_at = sqlc.arg(now),
       expires_at = GREATEST(expires_at, sqlc.arg(expires_at)::timestamptz),
       source_session_id = sqlc.narg(source_session_id),
       source_turn_ids = sqlc.arg(source_turn_ids)::uuid[],
       extraction_prompt_version_id = COALESCE(sqlc.narg(extraction_prompt_version_id), extraction_prompt_version_id),
       updated_at = sqlc.arg(now)
 WHERE user_id = sqlc.arg(user_id) AND id = sqlc.arg(id) AND status = 'ACTIVE';

-- superseded_by is DEFERRABLE INITIALLY DEFERRED: the new row is inserted after this UPDATE.
-- name: SupersedeMemory :execrows
UPDATE memories SET status = 'SUPERSEDED', superseded_by = sqlc.arg(new_id),
       expires_at = sqlc.arg(expires_at), updated_at = sqlc.arg(now)
 WHERE user_id = sqlc.arg(user_id) AND id = sqlc.arg(id) AND status = 'ACTIVE';

-- name: HasActiveTombstone :one
SELECT EXISTS (
    SELECT 1 FROM memories
     WHERE user_id = sqlc.arg(user_id) AND category = sqlc.arg(category) AND memory_key = sqlc.arg(memory_key)
       AND status = 'DELETED' AND expires_at > sqlc.arg(now)::timestamptz
)::boolean AS active;

-- name: CountActiveByGroup :one
SELECT count(*)::int FROM memories
 WHERE user_id = sqlc.arg(user_id) AND status = 'ACTIVE' AND category = ANY(sqlc.arg(categories)::text[]);

-- Cap eviction (spec 11.8.2): lowest 0.6*importance + 0.4*confidence first (by_score), else the
-- oldest observation; ties by last_observed_at, then id.
-- name: DeleteLowestInGroup :execrows
DELETE FROM memories AS d
 WHERE d.user_id = sqlc.arg(user_id)
   AND d.id IN (
       SELECT m.id FROM memories m
        WHERE m.user_id = sqlc.arg(user_id) AND m.status = 'ACTIVE' AND m.category = ANY(sqlc.arg(categories)::text[])
        ORDER BY CASE WHEN sqlc.arg(by_score)::boolean THEN 0.6 * m.importance + 0.4 * m.confidence ELSE 0 END,
                 m.last_observed_at, m.id
        LIMIT sqlc.arg(n)::int);

-- List excludes the embedding (the API never shows it). Order: category, last_observed_at DESC, id.
-- name: ListMemories :many
SELECT id, user_id, category, memory_key, content, display_text, evidence_type, confidence, importance,
       observation_count, source_session_id, source_turn_ids, extraction_prompt_version_id, embedding_model,
       status, superseded_by, first_observed_at, last_observed_at, expires_at, created_at, updated_at
  FROM memories
 WHERE user_id = sqlc.arg(user_id)
   AND status = 'ACTIVE'
   AND expires_at > sqlc.arg(now)::timestamptz
   AND (sqlc.narg(category)::text IS NULL OR category = sqlc.narg(category)::text)
   AND (sqlc.narg(after_category)::text IS NULL
        OR category > sqlc.narg(after_category)::text
        OR (category = sqlc.narg(after_category)::text
            AND (last_observed_at < sqlc.narg(after_observed_at)::timestamptz
                 OR (last_observed_at = sqlc.narg(after_observed_at)::timestamptz
                     AND id > sqlc.narg(after_id)::uuid))))
 ORDER BY category, last_observed_at DESC, id
 LIMIT sqlc.arg(max_rows)::int;

-- name: GetActiveForUpdate :one
SELECT id, category, memory_key FROM memories
 WHERE user_id = sqlc.arg(user_id) AND id = sqlc.arg(id) AND status = 'ACTIVE'
 FOR UPDATE;

-- Versions behind a memory (spec 11.9): both the anchor and the recursive step stay in the user.
-- name: DeleteSupersedeChain :execrows
WITH RECURSIVE chain AS (
    SELECT c0.id FROM memories c0
     WHERE c0.superseded_by = sqlc.arg(id) AND c0.user_id = sqlc.arg(user_id)
    UNION ALL
    SELECT x.id FROM memories x JOIN chain c ON x.superseded_by = c.id
     WHERE x.user_id = sqlc.arg(user_id)
)
DELETE FROM memories AS d
 WHERE d.id IN (SELECT chain.id FROM chain) AND d.user_id = sqlc.arg(user_id);

-- name: DeleteMemoryByID :execrows
DELETE FROM memories WHERE id = sqlc.arg(id) AND user_id = sqlc.arg(user_id);

-- name: UpsertTombstone :exec
INSERT INTO memories (id, user_id, category, memory_key, content, display_text, evidence_type,
                      confidence, importance, observation_count, embedding, status,
                      first_observed_at, last_observed_at, expires_at, created_at, updated_at)
VALUES (sqlc.arg(id), sqlc.arg(user_id), sqlc.arg(category), sqlc.arg(memory_key), '', '', 'USER_DELETED',
        0, 0, 1, NULL, 'DELETED', sqlc.arg(now), sqlc.arg(now), sqlc.arg(expires_at), sqlc.arg(now), sqlc.arg(now))
ON CONFLICT (user_id, category, memory_key) WHERE status = 'DELETED'
DO UPDATE SET expires_at = EXCLUDED.expires_at, updated_at = EXCLUDED.updated_at;

-- name: DeleteAllMemories :execrows
DELETE FROM memories WHERE user_id = sqlc.arg(user_id);

-- name: ListExistingForPrompt :many
SELECT category, memory_key, content FROM memories
 WHERE user_id = sqlc.arg(user_id)
   AND status = 'ACTIVE'
   AND category IN ('PROFILE_FACT', 'PREFERENCE', 'LEARNING_PATTERN')
   AND expires_at > sqlc.arg(now)::timestamptz
 ORDER BY last_observed_at DESC, id
 LIMIT sqlc.arg(max_rows)::int;
