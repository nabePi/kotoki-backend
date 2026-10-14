package httpapi

import (
	"context"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/nabePi/kotoki-backend/internal/admin"
	"github.com/nabePi/kotoki-backend/internal/auth"
	"github.com/nabePi/kotoki-backend/internal/balance"
	"github.com/nabePi/kotoki-backend/internal/platform/clock"
)

type adminHandler struct {
	svc adminService
	clk clock.Clock
}

func (h *adminHandler) listUsers(ctx context.Context, req Request[struct{}]) (Page[adminUserResponse], error) {
	q := req.Query()
	limit, err := ParseLimit(q)
	if err != nil {
		return Page[adminUserResponse]{}, err
	}
	f := auth.UserFilter{EmailPrefix: strings.TrimSpace(q.Get("email"))}
	if f.EmailPrefix != "" && len([]rune(f.EmailPrefix)) < 3 {
		return Page[adminUserResponse]{}, FieldError("email", "out_of_range")
	}
	if s := q.Get("status"); s != "" {
		st := auth.UserStatus(s)
		if st != auth.UserStatusActive && st != auth.UserStatusSuspended && st != auth.UserStatusDeleted {
			return Page[adminUserResponse]{}, FieldError("status", "not_allowed")
		}
		f.Status = &st
	}
	if r := q.Get("role"); r != "" {
		role := auth.Role(r)
		if role != auth.RoleUser && role != auth.RoleAdmin {
			return Page[adminUserResponse]{}, FieldError("role", "not_allowed")
		}
		f.Role = &role
	}
	after, err := parseUUIDCursor(q.Get("cursor"))
	if err != nil {
		return Page[adminUserResponse]{}, err
	}
	views, next, err := h.svc.ListUsers(ctx, f, after, limit)
	if err != nil {
		return Page[adminUserResponse]{}, err
	}
	items := make([]adminUserResponse, len(views))
	for i, v := range views {
		items[i] = newAdminUserResponse(v)
	}
	return NewPage(items, nextCursor(next)), nil
}

func (h *adminHandler) getUser(ctx context.Context, req Request[struct{}]) (adminUserDetailResponse, error) {
	id, err := req.PathUUID("user_id")
	if err != nil {
		return adminUserDetailResponse{}, err
	}
	d, err := h.svc.GetUser(ctx, id)
	if err != nil {
		return adminUserDetailResponse{}, err
	}
	return adminUserDetailResponse{
		adminUserResponse: newAdminUserResponse(d.UserView), OpenSessionID: uuidPtrString(d.OpenSessionID),
		ActiveRefreshTokenCount: d.ActiveRefreshTokenCount,
	}, nil
}

func (h *adminHandler) patchUser(ctx context.Context, req Request[patchUserRequest]) (adminUserResponse, error) {
	id, err := req.PathUUID("user_id")
	if err != nil {
		return adminUserResponse{}, err
	}
	view, err := h.svc.ChangeStatus(ctx, admin.ChangeStatusInput{
		AdminID: req.Principal.UserID, TargetID: id, To: auth.UserStatus(req.Body.Status), Reason: req.Body.Reason,
		Meta: metaOf(req.RequestID, req.ClientIP, req.UserAgent),
	})
	if err != nil {
		return adminUserResponse{}, err
	}
	return newAdminUserResponse(view), nil
}

func (h *adminHandler) grant(ctx context.Context, req Request[grantRequest]) (movementResponse, error) {
	id, err := req.PathUUID("user_id")
	if err != nil {
		return movementResponse{}, err
	}
	if err := balance.ValidateAdminIdempotencyKey(req.IdempotencyKey); err != nil {
		return movementResponse{}, err
	}
	res, err := h.svc.Grant(ctx, admin.GrantRequest{
		GrantInput: balance.GrantInput{UserID: id, AdminUserID: req.Principal.UserID, Seconds: req.Body.Seconds,
			Reason: req.Body.Reason, IdempotencyKey: req.IdempotencyKey, At: h.clk.Now()},
		Meta: metaOf(req.RequestID, req.ClientIP, req.UserAgent),
	})
	return h.movement(req, res, err)
}

func (h *adminHandler) adjust(ctx context.Context, req Request[adjustRequest]) (movementResponse, error) {
	id, err := req.PathUUID("user_id")
	if err != nil {
		return movementResponse{}, err
	}
	if err := balance.ValidateAdminIdempotencyKey(req.IdempotencyKey); err != nil {
		return movementResponse{}, err
	}
	res, err := h.svc.Adjust(ctx, admin.AdjustRequest{
		AdjustInput: balance.AdjustInput{UserID: id, AdminUserID: req.Principal.UserID, SecondsDelta: req.Body.SecondsDelta,
			Reason: req.Body.Reason, IdempotencyKey: req.IdempotencyKey, At: h.clk.Now()},
		Meta: metaOf(req.RequestID, req.ClientIP, req.UserAgent),
	})
	return h.movement(req, res, err)
}

// movement maps a grant/adjustment result: a replay answers 200 with Idempotent-Replayed.
func (h *adminHandler) movement(req interface {
	SetStatus(int)
	SetHeader(k, v string)
}, res balance.MovementResult, err error) (movementResponse, error) {
	if err != nil {
		return movementResponse{}, err
	}
	if res.Replayed {
		req.SetStatus(http.StatusOK)
		req.SetHeader("Idempotent-Replayed", "true")
	}
	return newMovementResponse(res), nil
}

func (h *adminHandler) transactions(ctx context.Context, req Request[struct{}]) (Page[ledgerEntryAdminResponse], error) {
	id, err := req.PathUUID("user_id")
	if err != nil {
		return Page[ledgerEntryAdminResponse]{}, err
	}
	q := req.Query()
	limit, err := ParseLimit(q)
	if err != nil {
		return Page[ledgerEntryAdminResponse]{}, err
	}
	types, err := parseEntryTypes(q.Get("entry_type"))
	if err != nil {
		return Page[ledgerEntryAdminResponse]{}, err
	}
	after, err := parseUUIDCursor(q.Get("cursor"))
	if err != nil {
		return Page[ledgerEntryAdminResponse]{}, err
	}
	entries, next, err := h.svc.ListTransactions(ctx, id, types, after, limit)
	if err != nil {
		return Page[ledgerEntryAdminResponse]{}, err
	}
	items := make([]ledgerEntryAdminResponse, len(entries))
	for i, e := range entries {
		items[i] = newLedgerEntryAdminResponse(e)
	}
	return NewPage(items, nextCursor(next)), nil
}

func (h *adminHandler) auditLogs(ctx context.Context, req Request[struct{}]) (Page[auditLogResponse], error) {
	q := req.Query()
	limit, err := ParseLimit(q)
	if err != nil {
		return Page[auditLogResponse]{}, err
	}
	f := admin.AuditFilter{Action: q.Get("action")}
	for key, dst := range map[string]**uuid.UUID{"admin_user_id": &f.AdminUserID, "target_user_id": &f.TargetUserID} {
		if v := q.Get(key); v != "" {
			id, perr := uuid.Parse(v)
			if perr != nil {
				return Page[auditLogResponse]{}, FieldError(key, "invalid_format")
			}
			*dst = &id
		}
	}
	after, err := parseUUIDCursor(q.Get("cursor"))
	if err != nil {
		return Page[auditLogResponse]{}, err
	}
	rows, next, err := h.svc.ListAudit(ctx, f, after, limit)
	if err != nil {
		return Page[auditLogResponse]{}, err
	}
	items := make([]auditLogResponse, len(rows))
	for i, a := range rows {
		items[i] = newAuditLogResponse(a)
	}
	return NewPage(items, nextCursor(next)), nil
}
