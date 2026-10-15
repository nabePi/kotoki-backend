-- Session views for REST: names of the parents come from topics and scenarios, the character name and
-- avatar from the immutable config_snapshot (never from the live characters row, D-014).

-- name: GetSessionView :one
SELECT sqlc.embed(s),
       t.name  AS topic_name,
       sc.name AS scenario_name,
       COALESCE(s.config_snapshot->'character'->>'name', '')::text AS character_name,
       COALESCE(s.config_snapshot->'character'->>'avatar_key', '')::text AS avatar_key,
       r.start_deadline_at
  FROM sessions s
  JOIN topics t     ON t.id  = s.topic_id
  JOIN scenarios sc ON sc.id = s.scenario_id
  LEFT JOIN balance_reservations r ON r.session_id = s.id
 WHERE s.id = sqlc.arg(id) AND s.user_id = sqlc.arg(user_id);

-- name: ListSessionViews :many
SELECT sqlc.embed(s),
       t.name  AS topic_name,
       sc.name AS scenario_name,
       COALESCE(s.config_snapshot->'character'->>'name', '')::text AS character_name,
       COALESCE(s.config_snapshot->'character'->>'avatar_key', '')::text AS avatar_key,
       r.start_deadline_at
  FROM sessions s
  JOIN topics t     ON t.id  = s.topic_id
  JOIN scenarios sc ON sc.id = s.scenario_id
  LEFT JOIN balance_reservations r ON r.session_id = s.id
 WHERE s.user_id = sqlc.arg(user_id)
   AND (sqlc.narg(statuses)::text[] IS NULL OR s.status = ANY(sqlc.narg(statuses)::text[]))
   AND (sqlc.narg(after)::uuid IS NULL OR s.id < sqlc.narg(after)::uuid)
 ORDER BY s.id DESC
 LIMIT sqlc.arg(row_limit)::int;
