-- name: InsertUser :exec
INSERT INTO users (id, email, phone_number, full_name, password_hash, role, status, password_changed_at,
                   last_login_at, terms_version, terms_accepted_at, created_at, updated_at, deleted_at, purged_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15);

-- name: GetUserByID :one
SELECT * FROM users WHERE id = $1;

-- name: GetUserStatus :one
SELECT status FROM users WHERE id = $1;

-- name: GetUserByEmail :one
SELECT * FROM users WHERE email = $1;

-- name: LockUserByID :one
SELECT * FROM users WHERE id = $1 FOR UPDATE;

-- name: UpdateUserLastLogin :exec
UPDATE users SET last_login_at = sqlc.arg(at)::timestamptz, updated_at = sqlc.arg(at)::timestamptz WHERE id = sqlc.arg(id);

-- name: SetUserPasswordHash :exec
UPDATE users
   SET password_hash       = sqlc.arg(password_hash),
       password_changed_at = COALESCE(sqlc.narg(changed_at)::timestamptz, password_changed_at),
       updated_at          = COALESCE(sqlc.narg(changed_at)::timestamptz, updated_at)
 WHERE id = sqlc.arg(id);

-- name: UpdateUserConsents :one
UPDATE users
   SET terms_accepted_at = CASE WHEN terms_version <> sqlc.arg(terms_version)::text THEN sqlc.arg(at)::timestamptz ELSE terms_accepted_at END,
       terms_version     = sqlc.arg(terms_version)::text,
       updated_at        = sqlc.arg(at)::timestamptz
 WHERE id = sqlc.arg(id) AND status = 'ACTIVE'
RETURNING *;

-- name: UpdateUserStatus :exec
UPDATE users
   SET status     = sqlc.arg(status)::text,
       updated_at = sqlc.arg(at)::timestamptz,
       deleted_at = CASE WHEN sqlc.arg(status)::text = 'DELETED' THEN sqlc.arg(at)::timestamptz ELSE NULL END
 WHERE id = sqlc.arg(id);

-- name: ListUsers :many
SELECT * FROM users
 WHERE (sqlc.arg(email_prefix)::text = '' OR starts_with(lower(email::text), lower(sqlc.arg(email_prefix)::text)))
   AND (sqlc.narg(status)::text IS NULL OR status = sqlc.narg(status)::text)
   AND (sqlc.narg(role)::text IS NULL OR role = sqlc.narg(role)::text)
   AND (sqlc.narg(after)::uuid IS NULL OR id < sqlc.narg(after)::uuid)
 ORDER BY id DESC
 LIMIT sqlc.arg(row_limit)::int;
