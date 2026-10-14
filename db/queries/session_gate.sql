-- name: ListOpenSessions :many
SELECT id, status FROM sessions
 WHERE user_id = $1 AND status IN ('RESERVED', 'STARTING', 'ACTIVE', 'ENDING')
 ORDER BY id;

-- name: GetTicketTarget :one
SELECT s.status, r.start_deadline_at
  FROM sessions s JOIN balance_reservations r ON r.session_id = s.id
 WHERE s.id = $1 AND s.user_id = $2;
