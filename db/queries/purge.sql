-- Purge akun (spec 13.9.3, 9.9.8). Dipakai job account_purge lewat postgres.PurgeRepo. Tiap langkah idempoten.

-- name: ListPurgeCandidates :many
SELECT id FROM users
WHERE status = 'DELETED' AND purged_at IS NULL AND deleted_at < sqlc.arg(deleted_before)::timestamptz
ORDER BY deleted_at, id
LIMIT sqlc.arg(max_users)::int;

-- name: PurgeDeleteMemories :execrows
DELETE FROM memories WHERE user_id = $1;

-- name: PurgeDeleteFindings :execrows
DELETE FROM evaluation_findings WHERE user_id = $1;

-- name: PurgeDeleteEvaluations :execrows
DELETE FROM evaluations WHERE user_id = $1;

-- name: PurgeDeleteProficiencyEstimates :execrows
DELETE FROM proficiency_estimates WHERE user_id = $1;

-- name: PurgeDeleteSkillStates :execrows
DELETE FROM learner_skill_states WHERE user_id = $1;

-- name: PurgeDeleteLearnerProfile :execrows
DELETE FROM learner_profiles WHERE user_id = $1;

-- name: PurgeSessionIDs :many
SELECT id FROM sessions WHERE user_id = $1 ORDER BY id;

-- name: PurgeDeleteTurns :execrows
-- (id, created_at) agar baris partisi yang dituju dihapus tepat; session_turns dipartisi menurut created_at.
DELETE FROM session_turns
WHERE (id, created_at) IN (SELECT t.id, t.created_at FROM session_turns t
                           WHERE t.session_id = ANY(sqlc.arg(session_ids)::uuid[]) LIMIT sqlc.arg(batch)::int);

-- name: PurgeDeleteSessionEvents :execrows
DELETE FROM session_events
WHERE (id, created_at) IN (SELECT e.id, e.created_at FROM session_events e
                           WHERE e.session_id = ANY(sqlc.arg(session_ids)::uuid[]) LIMIT sqlc.arg(batch)::int);

-- name: PurgeClearSummaries :execrows
UPDATE sessions SET summary_text = NULL, updated_at = now()
WHERE user_id = $1 AND summary_text IS NOT NULL;

-- name: PurgeAnonymizeUser :execrows
-- Langkah terakhir: purged_at hanya diisi di sini. Guard status/purged_at membuat panggilan ulang menjadi no-op.
UPDATE users
SET email = 'deleted+' || id::text || '@deleted.invalid', phone_number = NULL, full_name = 'deleted',
    password_hash = '!', purged_at = sqlc.arg(purged_at)::timestamptz, updated_at = sqlc.arg(purged_at)::timestamptz
WHERE id = sqlc.arg(user_id) AND status = 'DELETED' AND purged_at IS NULL;

-- name: PurgeUserIsPurged :one
SELECT (purged_at IS NOT NULL)::boolean AS purged FROM users WHERE id = $1;
