-- Summary worker (spec 9.9.6).

-- name: GetSessionForSummary :one
SELECT s.id, s.user_id, s.starting_at, s.started_at, s.ended_at,
       t.name AS topic_name, sc.name AS scenario_name, u.status AS user_status
  FROM sessions s
  JOIN users u ON u.id = s.user_id
  JOIN topics t ON t.id = s.topic_id
  JOIN scenarios sc ON sc.id = s.scenario_id
 WHERE s.id = $1;

-- name: ListTurnsForSummary :many
SELECT sequence_number, status, user_text, assistant_text, assistant_spoken_text
  FROM session_turns
 WHERE session_id = sqlc.arg(session_id) AND status IS DISTINCT FROM 'DISCARDED'
   AND created_at >= sqlc.arg(created_from)::timestamptz AND created_at <= sqlc.arg(created_to)::timestamptz
 ORDER BY sequence_number;

-- Not a transition: neither version nor runtime_fence change, and an existing summary is never
-- replaced.
-- name: SetSessionSummaryIfNull :execrows
UPDATE sessions SET summary_text = sqlc.arg(text), updated_at = sqlc.arg(at)
 WHERE id = sqlc.arg(id) AND summary_text IS NULL;
