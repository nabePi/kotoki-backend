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

-- name: LockReservationBySession :one
SELECT * FROM balance_reservations WHERE session_id = $1 FOR UPDATE;

-- name: InsertReservation :exec
INSERT INTO balance_reservations (id, user_id, account_id, session_id, reserved_seconds, status, start_deadline_at, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, 'HELD', $6, $7, $7);

-- name: MarkReservationSettled :execrows
UPDATE balance_reservations
   SET status = 'SETTLED', consumed_seconds = sqlc.arg(consumed)::bigint, released_seconds = sqlc.arg(released)::bigint,
       settled_at = sqlc.arg(at)::timestamptz, updated_at = sqlc.arg(at)::timestamptz
 WHERE id = sqlc.arg(id) AND status = 'HELD';

-- name: MarkReservationReleased :execrows
UPDATE balance_reservations
   SET status = 'RELEASED', consumed_seconds = 0, released_seconds = reserved_seconds,
       released_at = sqlc.arg(at)::timestamptz, updated_at = sqlc.arg(at)::timestamptz
 WHERE id = sqlc.arg(id) AND status = 'HELD';

-- Reconciliation (spec 4.8). Each query returns the offending rows as one short text (ids and
-- numbers only, no personal data); an empty result means the invariant holds.

-- name: ReconcileR1 :many
SELECT ('user_id=' || a.user_id::text || ' available=' || a.available_seconds::text || '/' || COALESCE(l.sum_available, 0)::text
        || ' reserved=' || a.reserved_seconds::text || '/' || COALESCE(l.sum_reserved, 0)::text)::text AS row
  FROM balance_accounts a
  LEFT JOIN (SELECT account_id,
                    SUM(available_seconds_delta) AS sum_available,
                    SUM(reserved_seconds_delta)  AS sum_reserved
               FROM balance_ledger GROUP BY account_id) l ON l.account_id = a.id
 WHERE a.available_seconds <> COALESCE(l.sum_available, 0)
    OR a.reserved_seconds  <> COALESCE(l.sum_reserved, 0);

-- name: ReconcileR2 :many
SELECT ('user_id=' || a.user_id::text || ' reserved=' || a.reserved_seconds::text || ' held=' || COALESCE(h.held, 0)::text)::text AS row
  FROM balance_accounts a
  LEFT JOIN (SELECT account_id, SUM(reserved_seconds) AS held
               FROM balance_reservations WHERE status = 'HELD' GROUP BY account_id) h
         ON h.account_id = a.id
 WHERE a.reserved_seconds <> COALESCE(h.held, 0);

-- name: ReconcileR3 :many
SELECT ('user_id=' || a.user_id::text)::text AS row
  FROM balance_accounts a
  LEFT JOIN LATERAL (SELECT available_seconds_after, reserved_seconds_after, account_version
                       FROM balance_ledger WHERE account_id = a.id
                      ORDER BY account_version DESC LIMIT 1) last ON true
 WHERE COALESCE(last.account_version, 0)          <> a.version
    OR COALESCE(last.available_seconds_after, 0)  <> a.available_seconds
    OR COALESCE(last.reserved_seconds_after, 0)   <> a.reserved_seconds;

-- name: ReconcileR4 :many
SELECT ('reservation_id=' || r.id::text || ' reservation=' || r.status || ' session=' || s.status)::text AS row
  FROM balance_reservations r JOIN sessions s ON s.id = r.session_id
 WHERE NOT (
       (r.status = 'HELD'     AND s.status IN ('RESERVED','STARTING','ACTIVE','ENDING'))
    OR (r.status = 'RELEASED' AND (s.status IN ('CANCELLED','ABANDONED')
                                   OR (s.status = 'FAILED' AND s.started_at IS NULL)))
    OR (r.status = 'SETTLED'  AND s.status IN ('COMPLETED','FAILED') AND s.started_at IS NOT NULL));

-- name: ReconcileR5 :many
SELECT ('reservation_id=' || r.id::text)::text AS row
  FROM balance_reservations r
  JOIN sessions s ON s.id = r.session_id
  LEFT JOIN (SELECT reservation_id,
                    SUM(reserved_seconds_delta) AS sum_reserved,
                    -SUM(reserved_seconds_delta) FILTER (WHERE entry_type = 'CONSUME') AS consumed
               FROM balance_ledger WHERE reservation_id IS NOT NULL GROUP BY reservation_id) l
         ON l.reservation_id = r.id
 WHERE (r.status <> 'HELD' AND (s.charged_seconds <> r.consumed_seconds
                                OR COALESCE(l.sum_reserved, 0) <> 0
                                OR COALESCE(l.consumed, 0) <> r.consumed_seconds))
    OR (r.status = 'HELD' AND COALESCE(l.sum_reserved, 0) <> r.reserved_seconds);

-- name: ReconcileR6 :many
SELECT ('reservation_id=' || r.id::text)::text AS row
  FROM balance_reservations r JOIN sessions s ON s.id = r.session_id
 WHERE r.status = 'HELD' AND s.status = 'RESERVED' AND r.start_deadline_at < now() - interval '10 minutes';
