-- name: InsertAuditLog :exec
INSERT INTO admin_audit_logs (id, admin_user_id, action, target_type, target_id, target_user_id, request_id,
                              idempotency_key, reason, details, ip_address, user_agent, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13);

-- name: ListAuditLogs :many
SELECT * FROM admin_audit_logs
 WHERE (sqlc.narg(admin_user_id)::uuid IS NULL OR admin_user_id = sqlc.narg(admin_user_id)::uuid)
   AND (sqlc.narg(target_user_id)::uuid IS NULL OR target_user_id = sqlc.narg(target_user_id)::uuid)
   AND (sqlc.arg(action)::text = '' OR action = sqlc.arg(action)::text)
   AND (sqlc.narg(after)::uuid IS NULL OR id < sqlc.narg(after)::uuid)
 ORDER BY id DESC
 LIMIT sqlc.arg(row_limit)::int;
