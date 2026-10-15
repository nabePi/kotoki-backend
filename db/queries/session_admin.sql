-- Admin reads of sessions (spec 5.9.8): not filtered by owner, with the reservation joined in.

-- name: GetSessionAdminView :one
SELECT sqlc.embed(s),
       t.name  AS topic_name,
       sc.name AS scenario_name,
       COALESCE(s.config_snapshot->'character'->>'name', '')::text       AS character_name,
       COALESCE(s.config_snapshot->'character'->>'avatar_key', '')::text AS avatar_key,
       r.id AS res_id, r.account_id AS res_account_id, r.status AS res_status, r.reserved_seconds AS res_reserved_seconds,
       r.consumed_seconds AS res_consumed_seconds, r.released_seconds AS res_released_seconds,
       r.start_deadline_at AS res_start_deadline_at, r.created_at AS res_created_at,
       r.settled_at AS res_settled_at, r.released_at AS res_released_at
  FROM sessions s
  JOIN topics t     ON t.id  = s.topic_id
  JOIN scenarios sc ON sc.id = s.scenario_id
  LEFT JOIN balance_reservations r ON r.session_id = s.id
 WHERE s.id = sqlc.arg(id);

-- name: ListSessionAdminViews :many
SELECT sqlc.embed(s),
       t.name  AS topic_name,
       sc.name AS scenario_name,
       COALESCE(s.config_snapshot->'character'->>'name', '')::text       AS character_name,
       COALESCE(s.config_snapshot->'character'->>'avatar_key', '')::text AS avatar_key,
       r.id AS res_id, r.account_id AS res_account_id, r.status AS res_status, r.reserved_seconds AS res_reserved_seconds,
       r.consumed_seconds AS res_consumed_seconds, r.released_seconds AS res_released_seconds,
       r.start_deadline_at AS res_start_deadline_at, r.created_at AS res_created_at,
       r.settled_at AS res_settled_at, r.released_at AS res_released_at
  FROM sessions s
  JOIN topics t     ON t.id  = s.topic_id
  JOIN scenarios sc ON sc.id = s.scenario_id
  LEFT JOIN balance_reservations r ON r.session_id = s.id
 WHERE s.user_id = sqlc.arg(user_id)
   AND (sqlc.narg(statuses)::text[] IS NULL OR s.status = ANY(sqlc.narg(statuses)::text[]))
   AND (sqlc.narg(after)::uuid IS NULL OR s.id < sqlc.narg(after)::uuid)
 ORDER BY s.id DESC
 LIMIT sqlc.arg(row_limit)::int;

-- Every turn of a session, DISCARDED and in-progress ones included, bounded by created_at for
-- partition pruning (spec 5.3.2).
-- name: ListAllTurnsForAdmin :many
SELECT * FROM session_turns
 WHERE session_id = sqlc.arg(session_id)
   AND created_at >= sqlc.arg(since)::timestamptz
   AND (sqlc.narg(until)::timestamptz IS NULL OR created_at <= sqlc.narg(until)::timestamptz)
   AND (sqlc.narg(after_seq)::int IS NULL OR sequence_number > sqlc.narg(after_seq)::int)
 ORDER BY sequence_number
 LIMIT sqlc.arg(row_limit)::int;

-- Keyset on (created_at, id) without a subquery by id, so partition pruning keeps working (S-6).
-- name: ListSessionEventsPage :many
SELECT * FROM session_events
 WHERE session_id = sqlc.arg(session_id)
   AND (sqlc.narg(after_at)::timestamptz IS NULL OR (created_at, id) > (sqlc.narg(after_at)::timestamptz, sqlc.narg(after_id)::uuid))
 ORDER BY created_at, id
 LIMIT sqlc.arg(row_limit)::int;
