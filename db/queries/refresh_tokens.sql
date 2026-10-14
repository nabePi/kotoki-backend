-- name: InsertRefreshToken :exec
INSERT INTO refresh_tokens (id, user_id, family_id, parent_id, token_hash, user_agent, ip_address, created_at, expires_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9);

-- name: GetRefreshTokenByID :one
SELECT * FROM refresh_tokens WHERE id = $1;

-- name: LockRefreshTokenByHash :one
SELECT * FROM refresh_tokens WHERE token_hash = $1 FOR UPDATE;

-- name: MarkRefreshTokenRotated :exec
UPDATE refresh_tokens
   SET revoked_at = sqlc.arg(at)::timestamptz, revoke_reason = 'ROTATED', replaced_by_id = sqlc.arg(replaced_by)::uuid
 WHERE id = sqlc.arg(id);

-- name: RevokeRefreshFamily :execrows
UPDATE refresh_tokens
   SET revoked_at = sqlc.arg(at)::timestamptz, revoke_reason = sqlc.arg(reason)::text
 WHERE family_id = sqlc.arg(family_id) AND revoked_at IS NULL;

-- name: RevokeAllRefreshForUser :execrows
UPDATE refresh_tokens
   SET revoked_at = sqlc.arg(at)::timestamptz, revoke_reason = sqlc.arg(reason)::text
 WHERE user_id = sqlc.arg(user_id) AND revoked_at IS NULL;

-- name: DeleteAllRefreshForUser :exec
DELETE FROM refresh_tokens WHERE user_id = $1;

-- name: CountActiveRefresh :one
SELECT count(*) FROM refresh_tokens
 WHERE user_id = sqlc.arg(user_id) AND revoked_at IS NULL AND expires_at > sqlc.arg(now)::timestamptz;
