package postgres

import (
	"context"
	"encoding/json"

	"github.com/google/uuid"

	"github.com/nabePi/kotoki-backend/internal/admin"
	"github.com/nabePi/kotoki-backend/internal/postgres/sqlcgen"
)

type auditRepo struct{ q *sqlcgen.Queries }

var _ admin.AuditRepository = (*auditRepo)(nil)

// Audit returns the transaction's audit repository.
func (t *Tx) Audit() admin.AuditRepository { return &auditRepo{q: t.Queries()} }

func (r *auditRepo) Insert(ctx context.Context, a admin.AuditLog) error {
	details := []byte("{}")
	if a.Details != nil {
		b, err := json.Marshal(a.Details)
		if err != nil {
			return internalErr(err, "encode audit details failed")
		}
		details = b
	}
	err := r.q.InsertAuditLog(ctx, sqlcgen.InsertAuditLogParams{
		ID: a.ID, AdminUserID: a.AdminUserID, Action: a.Action, TargetType: a.TargetType, TargetID: a.TargetID,
		TargetUserID: nullUUID(a.TargetUserID), RequestID: a.RequestID, IdempotencyKey: textPtr(a.IdempotencyKey),
		Reason: textPtr(a.Reason), Details: details, IpAddress: a.IP, UserAgent: textPtr(a.UserAgent), CreatedAt: tsz(a.CreatedAt),
	})
	if err != nil {
		return internalErr(err, "insert audit log failed")
	}
	return nil
}

func (r *auditRepo) List(ctx context.Context, f admin.AuditFilter, after *uuid.UUID, limit int) ([]admin.AuditLog, error) {
	rows, err := r.q.ListAuditLogs(ctx, sqlcgen.ListAuditLogsParams{
		AdminUserID: nullUUID(f.AdminUserID), TargetUserID: nullUUID(f.TargetUserID), Action: f.Action,
		After: nullUUID(after), RowLimit: int32(min(limit, 1<<30)), //nolint:gosec // bounded above
	})
	if err != nil {
		return nil, internalErr(err, "list audit logs failed")
	}
	out := make([]admin.AuditLog, len(rows))
	for i, l := range rows {
		var details map[string]any
		if len(l.Details) > 0 {
			if err := json.Unmarshal(l.Details, &details); err != nil {
				return nil, internalErr(err, "decode audit details failed")
			}
		}
		out[i] = admin.AuditLog{
			ID: l.ID, AdminUserID: l.AdminUserID, Action: l.Action, TargetType: l.TargetType, TargetID: l.TargetID,
			TargetUserID: fromNullUUID(l.TargetUserID), RequestID: l.RequestID, IdempotencyKey: fromText(l.IdempotencyKey),
			Reason: fromText(l.Reason), Details: details, IP: l.IpAddress, CreatedAt: fromTsz(l.CreatedAt),
		}
		if ua := fromText(l.UserAgent); ua != nil {
			out[i].UserAgent = ua
		}
	}
	return out, nil
}
