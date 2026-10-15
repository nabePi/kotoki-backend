-- Lifecycle events are written inside the transition's transaction, which holds the session lock.
-- name: InsertSessionEvent :exec
INSERT INTO session_events (id, session_id, event_type, actor, from_status, to_status, turn_id, payload, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9);

-- Runtime events (spec 7.13.1) only while the fence holds; no row means the fence is lost.
-- name: InsertSessionEventFenced :execrows
INSERT INTO session_events (id, session_id, event_type, actor, from_status, to_status, turn_id, payload, created_at)
SELECT sqlc.arg(id), sqlc.arg(session_id), sqlc.arg(event_type), sqlc.arg(actor), sqlc.narg(from_status), sqlc.narg(to_status),
       sqlc.narg(turn_id), sqlc.arg(payload), sqlc.arg(created_at)
 WHERE EXISTS (SELECT 1 FROM sessions s WHERE s.id = sqlc.arg(session_id) AND s.runtime_fence = sqlc.arg(fence)
                AND s.status IN ('STARTING', 'ACTIVE', 'ENDING'));

-- name: ListSessionEvents :many
SELECT * FROM session_events WHERE session_id = $1 ORDER BY created_at, id;
