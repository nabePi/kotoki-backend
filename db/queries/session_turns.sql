-- Every runtime write is fenced as in spec 7.9.3: the session must still have this fence and be
-- STARTING, ACTIVE or ENDING; no row means the fence is lost.

-- name: InsertTurnFenced :execrows
INSERT INTO session_turns (id, session_id, sequence_number, generation, created_at)
SELECT sqlc.arg(id), sqlc.arg(session_id), sqlc.arg(sequence_number), sqlc.arg(generation), sqlc.arg(created_at)
 WHERE EXISTS (SELECT 1 FROM sessions s WHERE s.id = sqlc.arg(session_id) AND s.runtime_fence = sqlc.arg(fence)
                AND s.status IN ('STARTING', 'ACTIVE', 'ENDING'));

-- name: UpdateTurnUserFenced :execrows
UPDATE session_turns t
   SET user_text = sqlc.arg(user_text), user_started_at = sqlc.narg(user_started_at), user_ended_at = sqlc.narg(user_ended_at),
       stt_provider = sqlc.narg(stt_provider), stt_final_ms = sqlc.narg(stt_final_ms), stt_audio_ms = sqlc.narg(stt_audio_ms)
 WHERE t.id = sqlc.arg(id) AND t.created_at = sqlc.arg(created_at) AND t.status IS NULL
   AND EXISTS (SELECT 1 FROM sessions s WHERE s.id = t.session_id AND s.runtime_fence = sqlc.arg(fence)
                AND s.status IN ('STARTING', 'ACTIVE', 'ENDING'));

-- name: FinalizeTurnFenced :execrows
UPDATE session_turns t
   SET status = sqlc.arg(status), assistant_text = sqlc.narg(assistant_text), assistant_spoken_text = sqlc.narg(assistant_spoken_text),
       llm_provider = sqlc.narg(llm_provider), llm_model = sqlc.narg(llm_model),
       tts_provider = sqlc.narg(tts_provider), tts_model = sqlc.narg(tts_model), tts_voice_id = sqlc.narg(tts_voice_id),
       used_fallback = sqlc.arg(used_fallback),
       turn_end_to_first_audio_ms = sqlc.narg(turn_end_to_first_audio_ms), llm_first_token_ms = sqlc.narg(llm_first_token_ms),
       tts_first_audio_ms = sqlc.narg(tts_first_audio_ms), input_tokens = sqlc.narg(input_tokens), output_tokens = sqlc.narg(output_tokens),
       tts_characters = sqlc.narg(tts_characters), error_code = sqlc.narg(error_code), completed_at = sqlc.arg(completed_at)
 WHERE t.id = sqlc.arg(id) AND t.created_at = sqlc.arg(created_at) AND t.status IS NULL
   AND EXISTS (SELECT 1 FROM sessions s WHERE s.id = t.session_id AND s.runtime_fence = sqlc.arg(fence)
                AND s.status IN ('STARTING', 'ACTIVE', 'ENDING'));

-- Takeover: every turn still in progress becomes FAILED (spec 5.3.2).
-- name: FailInProgressTurnsFenced :execrows
UPDATE session_turns t
   SET status = 'FAILED', completed_at = now(),
       error_code = COALESCE(sqlc.narg(error_code)::text, 'TAKEOVER'),
       assistant_text = COALESCE(sqlc.narg(assistant_text)::text, t.assistant_text),
       assistant_spoken_text = COALESCE(sqlc.narg(assistant_spoken_text)::text, t.assistant_spoken_text)
 WHERE t.session_id = sqlc.arg(session_id) AND t.status IS NULL
   AND t.created_at >= (SELECT s.starting_at - interval '1 minute' FROM sessions s WHERE s.id = sqlc.arg(session_id))
   AND EXISTS (SELECT 1 FROM sessions s WHERE s.id = sqlc.arg(session_id) AND s.runtime_fence = sqlc.arg(fence)
                AND s.status IN ('STARTING', 'ACTIVE', 'ENDING'));

-- Settle (spec 4.6.3 step 7): not fenced, the Service holds the session row lock.
-- name: FailDanglingTurns :execrows
UPDATE session_turns
   SET status = 'FAILED', completed_at = sqlc.arg(at), error_code = COALESCE(error_code, 'SESSION_ENDED')
 WHERE session_id = sqlc.arg(session_id) AND status IS NULL AND created_at >= sqlc.arg(since)::timestamptz;

-- name: ListRecentTurns :many
SELECT * FROM (
    SELECT * FROM session_turns
     WHERE session_id = sqlc.arg(session_id)
       AND created_at >= (SELECT s.starting_at - interval '1 minute' FROM sessions s WHERE s.id = sqlc.arg(session_id))
     ORDER BY sequence_number DESC
     LIMIT sqlc.arg(row_limit)::int
) recent ORDER BY sequence_number;

-- name: MaxTurnSequenceAndGeneration :one
SELECT COALESCE(MAX(sequence_number), -1)::int AS max_seq, COALESCE(MAX(generation), 0)::int AS max_gen
  FROM session_turns
 WHERE session_id = sqlc.arg(session_id)
   AND created_at >= (SELECT s.starting_at - interval '1 minute' FROM sessions s WHERE s.id = sqlc.arg(session_id));

-- name: TurnStats :one
SELECT count(*) FILTER (WHERE status IS DISTINCT FROM 'DISCARDED')::int AS turn_count,
       count(*) FILTER (WHERE status IS DISTINCT FROM 'DISCARDED' AND btrim(COALESCE(user_text, '')) <> '')::int AS user_turn_count,
       COALESCE(bool_or(stt_audio_ms IS NOT NULL OR tts_first_audio_ms IS NOT NULL), false)::boolean AS has_audio
  FROM session_turns
 WHERE session_id = sqlc.arg(session_id) AND created_at >= sqlc.arg(since)::timestamptz;

-- name: ListTurnsForUser :many
SELECT * FROM session_turns
 WHERE session_id = sqlc.arg(session_id)
   AND created_at >= sqlc.arg(since)::timestamptz
   AND (sqlc.narg(until)::timestamptz IS NULL OR created_at <= sqlc.narg(until)::timestamptz)
   AND (sqlc.narg(after_seq)::int IS NULL OR sequence_number > sqlc.narg(after_seq)::int)
   AND status IS DISTINCT FROM 'DISCARDED'
 ORDER BY sequence_number
 LIMIT sqlc.arg(row_limit)::int;
