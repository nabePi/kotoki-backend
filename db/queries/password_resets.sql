-- name: InvalidateOpenResets :exec
UPDATE password_reset_tokens SET invalidated_at = sqlc.arg(at)::timestamptz
 WHERE user_id = sqlc.arg(user_id) AND used_at IS NULL AND invalidated_at IS NULL;

-- name: InsertPasswordReset :exec
INSERT INTO password_reset_tokens (id, user_id, token_hash, requested_ip, created_at, expires_at)
VALUES ($1, $2, $3, $4, $5, $6);

-- name: FindPasswordResetByHash :one
SELECT t.*, u.email AS owner_email, u.status AS owner_status
  FROM password_reset_tokens t JOIN users u ON u.id = t.user_id
 WHERE t.token_hash = $1;

-- name: LockPasswordResetByHash :one
SELECT t.*, u.email AS owner_email, u.status AS owner_status
  FROM password_reset_tokens t JOIN users u ON u.id = t.user_id
 WHERE t.token_hash = $1
 FOR UPDATE OF t;

-- name: MarkResetUsed :exec
UPDATE password_reset_tokens SET used_at = sqlc.arg(at)::timestamptz WHERE id = sqlc.arg(id);

-- name: InvalidateOtherResets :exec
UPDATE password_reset_tokens SET invalidated_at = sqlc.arg(at)::timestamptz
 WHERE user_id = sqlc.arg(user_id) AND id <> sqlc.arg(except_id)
   AND used_at IS NULL AND invalidated_at IS NULL;

-- name: DeleteAllResetsForUser :exec
DELETE FROM password_reset_tokens WHERE user_id = $1;
