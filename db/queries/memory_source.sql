-- Transcript side of the memory extraction worker (spec 11.5.1 step 4). The memories table is
-- never touched here (memories.sql owns every access to it). The worker verifies
-- sessions.user_id against the event's user before it reads the turns.

-- name: ListTurnsForMemory :many
SELECT id, sequence_number, status, user_text, assistant_text, assistant_spoken_text
  FROM session_turns
 WHERE session_id = sqlc.arg(session_id) AND status IN ('COMPLETED', 'INTERRUPTED')
 ORDER BY sequence_number;
