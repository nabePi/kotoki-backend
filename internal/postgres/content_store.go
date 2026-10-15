package postgres

import (
	"context"
	"encoding/json"

	"github.com/nabePi/kotoki-backend/internal/content"
	"github.com/nabePi/kotoki-backend/internal/postgres/sqlcgen"
)

type contentStoreView struct{ s *Store }

// ContentStore returns the content.Store view of s: content reads and writes plus the admin audit
// log in one transaction.
func (s *Store) ContentStore() content.Store { return contentStoreView{s: s} }

func (v contentStoreView) WithinTx(ctx context.Context, fn func(ctx context.Context, tx content.Tx) error) error {
	return v.s.WithinTx(ctx, func(ctx context.Context, tx *Tx) error { return fn(ctx, contentTx{tx: tx}) })
}

type contentTx struct{ tx *Tx }

func (c contentTx) Content() content.TxRepository { return c.tx.Content() }
func (c contentTx) Audit() content.AuditWriter    { return contentAudit(c) }

type contentAudit struct{ tx *Tx }

// Record implements content.AuditWriter on admin_audit_logs.
func (a contentAudit) Record(ctx context.Context, e content.AuditEntry) error {
	details := []byte("{}")
	if e.Details != nil {
		b, err := json.Marshal(e.Details)
		if err != nil {
			return internalErr(err, "encode audit details failed")
		}
		details = b
	}
	err := a.tx.Queries().InsertAuditLog(ctx, sqlcgen.InsertAuditLogParams{
		ID: a.tx.IDs().New(), AdminUserID: e.AdminUserID, Action: e.Action, TargetType: e.TargetType, TargetID: e.TargetID,
		RequestID: e.RequestID, Details: details, CreatedAt: tsz(a.tx.Clock().Now()),
	})
	if err != nil {
		return internalErr(err, "insert audit log failed")
	}
	return nil
}
