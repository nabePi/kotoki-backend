package admin

import (
	"context"
	"errors"
	"log/slog"

	"github.com/google/uuid"

	"github.com/nabePi/kotoki-backend/internal/auth"
	"github.com/nabePi/kotoki-backend/internal/balance"
	"github.com/nabePi/kotoki-backend/internal/platform/clock"
	"github.com/nabePi/kotoki-backend/internal/platform/errs"
	"github.com/nabePi/kotoki-backend/internal/platform/idgen"
	"github.com/nabePi/kotoki-backend/internal/platform/otel"
)

// Service implements the admin use cases.
type Service struct {
	store Store
	bal   *balance.Service
	clk   clock.Clock
	ids   idgen.Generator
	log   *slog.Logger
}

// NewService returns a Service.
func NewService(store Store, bal *balance.Service, clk clock.Clock, ids idgen.Generator, log *slog.Logger) *Service {
	return &Service{store: store, bal: bal, clk: clk, ids: ids, log: log}
}

// GrantRequest is an admin grant plus the request context for the audit row.
type GrantRequest struct {
	balance.GrantInput
	Meta RequestMeta
}

// AdjustRequest is an admin adjustment plus the request context for the audit row.
type AdjustRequest struct {
	balance.AdjustInput
	Meta RequestMeta
}

func (s *Service) audit(adminID uuid.UUID, action string, target uuid.UUID, meta RequestMeta, idem, reason string, details map[string]any) AuditLog {
	a := AuditLog{
		ID: s.ids.New(), AdminUserID: adminID, Action: action, TargetType: TargetUser, TargetID: target.String(),
		TargetUserID: &target, RequestID: meta.RequestID, Details: details, IP: meta.IP, CreatedAt: s.clk.Now(),
	}
	if idem != "" {
		a.IdempotencyKey = &idem
	}
	if reason != "" {
		a.Reason = &reason
	}
	if meta.UserAgent != "" {
		ua := meta.UserAgent
		a.UserAgent = &ua
	}
	return a
}

func userNotFound(err error) error {
	if errors.Is(err, auth.ErrUserNotFound) {
		return errs.New(errs.CodeUserNotFound, "user not found")
	}
	return err
}

// retryDuplicate runs op and, if a concurrent request won the idempotency key,
// runs it once more so it can replay or reject.
func retryDuplicate(op func() error) error {
	err := op()
	if errors.Is(err, balance.ErrDuplicateMovement) {
		return op()
	}
	return err
}

// Grant adds seconds to a user and writes the audit row in the same transaction.
func (s *Service) Grant(ctx context.Context, in GrantRequest) (res balance.MovementResult, err error) {
	ctx, span := otel.Start(ctx, "admin.Grant")
	defer otel.End(span, &err)
	err = retryDuplicate(func() error {
		return s.store.WithinTx(ctx, func(ctx context.Context, tx Tx) error {
			if _, gerr := tx.Users().GetByID(ctx, in.UserID); gerr != nil {
				return userNotFound(gerr)
			}
			var gerr error
			res, gerr = s.bal.GrantTx(ctx, tx, in.GrantInput)
			if gerr != nil || res.Replayed {
				return gerr
			}
			return tx.Audit().Insert(ctx, s.audit(in.AdminUserID, ActionBalanceGrant, in.UserID, in.Meta, in.IdempotencyKey, in.Reason,
				map[string]any{"seconds": in.Seconds, "ledger_entry_id": res.Entry.ID.String(), "available_after": res.Entry.AvailableSecondsAfter}))
		})
	})
	return res, err
}

// Adjust changes a user's available seconds by a signed delta, with audit.
func (s *Service) Adjust(ctx context.Context, in AdjustRequest) (res balance.MovementResult, err error) {
	ctx, span := otel.Start(ctx, "admin.Adjust")
	defer otel.End(span, &err)
	err = retryDuplicate(func() error {
		return s.store.WithinTx(ctx, func(ctx context.Context, tx Tx) error {
			if _, gerr := tx.Users().GetByID(ctx, in.UserID); gerr != nil {
				return userNotFound(gerr)
			}
			var gerr error
			res, gerr = s.bal.AdjustTx(ctx, tx, in.AdjustInput)
			if gerr != nil || res.Replayed {
				return gerr
			}
			before := res.Entry.AvailableSecondsAfter - res.Entry.AvailableSecondsDelta
			return tx.Audit().Insert(ctx, s.audit(in.AdminUserID, ActionBalanceAdjustment, in.UserID, in.Meta, in.IdempotencyKey, in.Reason,
				map[string]any{"seconds_delta": in.SecondsDelta, "ledger_entry_id": res.Entry.ID.String(),
					"available_before": before, "available_after": res.Entry.AvailableSecondsAfter}))
		})
	})
	return res, err
}

// ListTransactions returns a user's ledger for admins; an unknown user is a 404.
func (s *Service) ListTransactions(ctx context.Context, userID uuid.UUID, types []balance.EntryType, after *uuid.UUID, limit int) ([]balance.LedgerEntry, *uuid.UUID, error) {
	var entries []balance.LedgerEntry
	var next *uuid.UUID
	err := s.store.WithinTx(ctx, func(ctx context.Context, tx Tx) error {
		if _, err := tx.Users().GetByID(ctx, userID); err != nil {
			return userNotFound(err)
		}
		var err error
		entries, err = tx.Balance().ListLedger(ctx, userID, types, after, limit+1)
		return err
	})
	if err != nil {
		return nil, nil, err
	}
	if len(entries) > limit {
		entries = entries[:limit]
		id := entries[limit-1].ID
		next = &id
	}
	return entries, next, nil
}

// ListAudit pages through the audit log, newest first.
func (s *Service) ListAudit(ctx context.Context, f AuditFilter, after *uuid.UUID, limit int) ([]AuditLog, *uuid.UUID, error) {
	var rows []AuditLog
	err := s.store.WithinTx(ctx, func(ctx context.Context, tx Tx) error {
		var err error
		rows, err = tx.Audit().List(ctx, f, after, limit+1)
		return err
	})
	if err != nil {
		return nil, nil, err
	}
	if len(rows) > limit {
		rows = rows[:limit]
		id := rows[limit-1].ID
		return rows, &id, nil
	}
	return rows, nil, nil
}

// IsActiveAdmin reports whether the user is an ACTIVE admin (re-checked in the DB per admin request).
func (s *Service) IsActiveAdmin(ctx context.Context, userID uuid.UUID) (bool, error) {
	var ok bool
	err := s.store.WithinTx(ctx, func(ctx context.Context, tx Tx) error {
		u, err := tx.Users().GetByID(ctx, userID)
		if errors.Is(err, auth.ErrUserNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		ok = u.Role == auth.RoleAdmin && u.Status == auth.UserStatusActive
		return nil
	})
	return ok, err
}

func validationError(field, reason string) error {
	return errs.WithDetails(errs.New(errs.CodeValidationFailed, "invalid "+field),
		map[string]any{"fields": []map[string]any{{"field": field, "reason": reason}}})
}
