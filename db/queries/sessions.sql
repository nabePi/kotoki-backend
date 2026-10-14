-- name: InsertSession :exec
INSERT INTO sessions (id, user_id, status, conversation_start_mode, duration_seconds, topic_id, scenario_id, scenario_version_id,
                      character_id, prompt_version_id, config_snapshot, adaptive_difficulty, reserved_seconds,
                      create_idempotency_key, create_request_hash, created_at, updated_at)
VALUES ($1, $2, 'RESERVED', $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $15);

-- name: GetSessionByID :one
SELECT * FROM sessions WHERE id = $1;

-- name: GetSessionForUpdate :one
SELECT * FROM sessions WHERE id = $1 FOR UPDATE;

-- name: GetSessionOwner :one
SELECT user_id FROM sessions WHERE id = $1;

-- name: FindSessionByIdempotencyKey :one
SELECT * FROM sessions WHERE user_id = $1 AND create_idempotency_key = $2;

-- name: FindOpenSessionByUser :one
SELECT * FROM sessions WHERE user_id = $1 AND status IN ('RESERVED', 'STARTING', 'ACTIVE', 'ENDING');

-- CAS of spec 5.2: one UPDATE, guarded by the from-status and, for the runtime, the fence. A nil
-- (NULL) argument leaves the column alone; version always goes up. config_snapshot is never written
-- after INSERT (D-064).
-- name: CASSession :one
UPDATE sessions SET
       status                    = sqlc.arg(to_status),
       end_reason                = COALESCE(sqlc.narg(end_reason)::text, end_reason),
       failure_detail            = COALESCE(sqlc.narg(failure_detail)::text, failure_detail),
       starting_at               = COALESCE(sqlc.narg(starting_at)::timestamptz, starting_at),
       started_at                = COALESCE(sqlc.narg(started_at)::timestamptz, started_at),
       expires_at                = COALESCE(sqlc.narg(expires_at)::timestamptz, expires_at),
       ending_at                 = COALESCE(sqlc.narg(ending_at)::timestamptz, ending_at),
       ended_at                  = COALESCE(sqlc.narg(ended_at)::timestamptz, ended_at),
       charged_seconds           = COALESCE(sqlc.narg(charged_seconds)::int, charged_seconds),
       opening_prompt_version_id = COALESCE(sqlc.narg(opening_prompt_version_id)::uuid, opening_prompt_version_id),
       runtime_fence             = runtime_fence + CASE WHEN sqlc.arg(bump_fence)::boolean THEN 1 ELSE 0 END,
       version                   = version + 1,
       updated_at                = sqlc.arg(at)
 WHERE id = sqlc.arg(id)
   AND status = sqlc.arg(from_status)
   AND (sqlc.narg(fence)::bigint IS NULL OR runtime_fence = sqlc.narg(fence)::bigint)
RETURNING *;

-- Spec 7.9.3: the runtime takes the session. Neither version nor updated_at move; handoff ends.
-- name: ClaimSession :one
UPDATE sessions SET runtime_fence = runtime_fence + 1, handoff_at = NULL
 WHERE id = sqlc.arg(id) AND status = ANY(sqlc.arg(allowed)::text[])
RETURNING *;

-- name: MarkSessionHandoff :execrows
UPDATE sessions SET handoff_at = sqlc.arg(at), updated_at = sqlc.arg(at)
 WHERE id = sqlc.arg(id) AND status = 'ACTIVE' AND runtime_fence = sqlc.arg(fence);

-- Spec 5.6.5. The part of the episode after expires_at is dropped.
-- name: AddSessionDisconnected :one
UPDATE sessions
   SET disconnected_seconds = LEAST(duration_seconds, disconnected_seconds + GREATEST(0,
         sqlc.arg(n)::int - GREATEST(0, ceil(extract(epoch FROM (sqlc.arg(at)::timestamptz - expires_at))))::int)),
       version = version + 1, updated_at = sqlc.arg(at)
 WHERE id = sqlc.arg(id) AND status IN ('ACTIVE', 'ENDING') AND runtime_fence = sqlc.arg(fence)
RETURNING *;

-- name: ListSessionsByUser :many
SELECT * FROM sessions
 WHERE user_id = sqlc.arg(user_id)
   AND (sqlc.narg(statuses)::text[] IS NULL OR status = ANY(sqlc.narg(statuses)::text[]))
   AND (sqlc.narg(after)::uuid IS NULL OR id < sqlc.narg(after)::uuid)
 ORDER BY id DESC
 LIMIT sqlc.arg(row_limit)::int;

-- Summary worker (Bab 9): not a transition, so neither version nor runtime_fence change.
-- name: SetSessionSummary :execrows
UPDATE sessions SET summary_text = sqlc.arg(text), updated_at = sqlc.arg(at) WHERE id = sqlc.arg(id);

-- Sweeper candidates (spec 5.6.4); none of them locks.
-- name: ListExpiredSessions :many
SELECT id FROM sessions WHERE status = 'ACTIVE' AND expires_at < $1 ORDER BY expires_at LIMIT $2;

-- name: ListStuckEndingSessions :many
SELECT id FROM sessions WHERE status = 'ENDING' AND ending_at < $1 ORDER BY ending_at LIMIT $2;

-- name: ListStuckStartingSessions :many
SELECT id FROM sessions WHERE status = 'STARTING' AND starting_at < $1 ORDER BY starting_at LIMIT $2;

-- name: ListHandoffStaleSessions :many
SELECT id FROM sessions WHERE status = 'ACTIVE' AND handoff_at < $1 ORDER BY handoff_at LIMIT $2;

-- name: ListActiveSessionsForHeartbeat :many
SELECT id, started_at, handoff_at FROM sessions
 WHERE status = 'ACTIVE' AND handoff_at IS NULL AND started_at < $1
 ORDER BY started_at LIMIT $2;

-- name: CountActiveNonHandoffSessions :one
SELECT count(*)::int FROM sessions WHERE status = 'ACTIVE' AND handoff_at IS NULL;

-- Spec 4.6.2: no FOR UPDATE, so the reservation is never locked before the account.
-- name: ListAbandonCandidates :many
SELECT r.session_id
  FROM balance_reservations r
  JOIN sessions s ON s.id = r.session_id
 WHERE r.status = 'HELD' AND s.status = 'RESERVED' AND r.start_deadline_at < $1
 ORDER BY r.start_deadline_at
 LIMIT $2;
