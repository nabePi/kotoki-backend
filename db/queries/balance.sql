-- name: InsertBalanceAccount :exec
INSERT INTO balance_accounts (id, user_id, available_seconds, reserved_seconds, version, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7);

-- name: GetBalanceAccountByUser :one
SELECT * FROM balance_accounts WHERE user_id = $1;

-- name: LockBalanceAccountByUser :one
SELECT * FROM balance_accounts WHERE user_id = $1 FOR UPDATE;

-- name: ApplyAccountDelta :one
UPDATE balance_accounts
   SET available_seconds = available_seconds + sqlc.arg(available_delta)::bigint,
       reserved_seconds  = reserved_seconds  + sqlc.arg(reserved_delta)::bigint,
       version           = version + 1,
       updated_at        = sqlc.arg(at)::timestamptz
 WHERE id = sqlc.arg(id)
RETURNING *;

-- name: InsertLedgerEntry :exec
INSERT INTO balance_ledger (
    id, user_id, account_id, entry_type, available_seconds_delta, reserved_seconds_delta,
    available_seconds_after, reserved_seconds_after, account_version, session_id, reservation_id,
    reference_type, admin_user_id, reason, idempotency_key, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16);

-- name: FindLedgerByKey :one
SELECT * FROM balance_ledger WHERE idempotency_key = $1;

-- name: ListLedgerForUser :many
SELECT * FROM balance_ledger
 WHERE user_id = sqlc.arg(user_id)
   AND (cardinality(sqlc.arg(types)::text[]) = 0 OR entry_type = ANY(sqlc.arg(types)::text[]))
   AND (sqlc.narg(after)::uuid IS NULL OR id < sqlc.narg(after)::uuid)
 ORDER BY id DESC
 LIMIT sqlc.arg(row_limit)::int;
