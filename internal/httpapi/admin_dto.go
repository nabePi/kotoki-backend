package httpapi

import (
	"net/netip"
	"strings"

	"github.com/nabePi/kotoki-backend/internal/admin"
	"github.com/nabePi/kotoki-backend/internal/auth"
	"github.com/nabePi/kotoki-backend/internal/balance"
)

type adminBalance struct {
	AvailableSeconds int64 `json:"available_seconds"`
	ReservedSeconds  int64 `json:"reserved_seconds"`
}

// adminUserResponse is the AdminUser object of spec 5.9.8.
type adminUserResponse struct {
	ID              string       `json:"id"`
	Email           string       `json:"email"`
	FullName        string       `json:"full_name"`
	PhoneNumber     *string      `json:"phone_number"`
	Role            string       `json:"role"`
	Status          string       `json:"status"`
	CreatedAt       string       `json:"created_at"`
	LastLoginAt     *string      `json:"last_login_at"`
	TermsVersion    string       `json:"terms_version"`
	TermsAcceptedAt string       `json:"terms_accepted_at"`
	DeletedAt       *string      `json:"deleted_at"`
	PurgedAt        *string      `json:"purged_at"`
	Balance         adminBalance `json:"balance"`
}

func newAdminUserResponse(v admin.UserView) adminUserResponse {
	u := v.User
	return adminUserResponse{
		ID: u.ID.String(), Email: u.Email, FullName: u.FullName, PhoneNumber: u.PhoneNumber, Role: string(u.Role),
		Status: string(u.Status), CreatedAt: formatTime(u.CreatedAt), LastLoginAt: formatTimePtr(u.LastLoginAt),
		TermsVersion: u.TermsVersion, TermsAcceptedAt: formatTime(u.TermsAcceptedAt), DeletedAt: formatTimePtr(u.DeletedAt),
		PurgedAt: formatTimePtr(u.PurgedAt),
		Balance:  adminBalance{AvailableSeconds: v.Balance.AvailableSeconds, ReservedSeconds: v.Balance.ReservedSeconds},
	}
}

type adminUserDetailResponse struct {
	adminUserResponse
	OpenSessionID           *string `json:"open_session_id"`
	ActiveRefreshTokenCount int     `json:"active_refresh_token_count"`
}

type patchUserRequest struct {
	Status string `json:"status"`
	Reason string `json:"reason"`
}

func (r patchUserRequest) Validate() error {
	switch auth.UserStatus(r.Status) {
	case auth.UserStatusActive, auth.UserStatusSuspended, auth.UserStatusDeleted:
	case "":
		return FieldError("status", "required")
	default:
		return FieldError("status", "not_allowed")
	}
	if n := len([]rune(r.Reason)); n < 3 || n > 500 {
		return FieldError("reason", "out_of_range")
	}
	return nil
}

type grantRequest struct {
	Seconds int64  `json:"seconds"`
	Reason  string `json:"reason"`
}

type adjustRequest struct {
	SecondsDelta int64  `json:"seconds_delta"`
	Reason       string `json:"reason"`
}

// ledgerEntryAdminResponse is the full ledger item shown to admins.
type ledgerEntryAdminResponse struct {
	ID                    string  `json:"id"`
	EntryType             string  `json:"entry_type"`
	AvailableSecondsDelta int64   `json:"available_seconds_delta"`
	ReservedSecondsDelta  int64   `json:"reserved_seconds_delta"`
	AvailableSecondsAfter int64   `json:"available_seconds_after"`
	ReservedSecondsAfter  int64   `json:"reserved_seconds_after"`
	ReferenceType         string  `json:"reference_type"`
	AdminUserID           *string `json:"admin_user_id"`
	Reason                *string `json:"reason"`
	IdempotencyKey        string  `json:"idempotency_key"`
	SessionID             *string `json:"session_id"`
	ReservationID         *string `json:"reservation_id"`
	CreatedAt             string  `json:"created_at"`
}

func newLedgerEntryAdminResponse(e balance.LedgerEntry) ledgerEntryAdminResponse {
	return ledgerEntryAdminResponse{
		ID: e.ID.String(), EntryType: string(e.EntryType), AvailableSecondsDelta: e.AvailableSecondsDelta,
		ReservedSecondsDelta: e.ReservedSecondsDelta, AvailableSecondsAfter: e.AvailableSecondsAfter,
		ReservedSecondsAfter: e.ReservedSecondsAfter, ReferenceType: string(e.ReferenceType),
		AdminUserID: uuidPtrString(e.AdminUserID), Reason: e.Reason, IdempotencyKey: e.IdempotencyKey,
		SessionID: uuidPtrString(e.SessionID), ReservationID: uuidPtrString(e.ReservationID), CreatedAt: formatTime(e.CreatedAt),
	}
}

type movementResponse struct {
	LedgerEntry ledgerEntryAdminResponse `json:"ledger_entry"`
	Balance     adminBalance             `json:"balance"`
}

func newMovementResponse(r balance.MovementResult) movementResponse {
	return movementResponse{
		LedgerEntry: newLedgerEntryAdminResponse(r.Entry),
		Balance:     adminBalance{AvailableSeconds: r.Account.AvailableSeconds, ReservedSeconds: r.Account.ReservedSeconds},
	}
}

type auditLogResponse struct {
	ID             string         `json:"id"`
	AdminUserID    string         `json:"admin_user_id"`
	Action         string         `json:"action"`
	TargetType     string         `json:"target_type"`
	TargetID       string         `json:"target_id"`
	TargetUserID   *string        `json:"target_user_id"`
	RequestID      string         `json:"request_id"`
	IdempotencyKey *string        `json:"idempotency_key"`
	Reason         *string        `json:"reason"`
	Details        map[string]any `json:"details"`
	IPAddress      *string        `json:"ip_address"`
	UserAgent      *string        `json:"user_agent"`
	CreatedAt      string         `json:"created_at"`
}

func newAuditLogResponse(a admin.AuditLog) auditLogResponse {
	var ip *string
	if a.IP != nil {
		s := a.IP.String()
		ip = &s
	}
	details := a.Details
	if details == nil {
		details = map[string]any{}
	}
	return auditLogResponse{
		ID: a.ID.String(), AdminUserID: a.AdminUserID.String(), Action: a.Action, TargetType: a.TargetType, TargetID: a.TargetID,
		TargetUserID: uuidPtrString(a.TargetUserID), RequestID: a.RequestID, IdempotencyKey: a.IdempotencyKey, Reason: a.Reason,
		Details: details, IPAddress: ip, UserAgent: a.UserAgent, CreatedAt: formatTime(a.CreatedAt),
	}
}

func metaOf(requestID string, ip netip.Addr, ua string) admin.RequestMeta {
	m := admin.RequestMeta{RequestID: requestID, UserAgent: strings.TrimSpace(ua)}
	if ip.IsValid() {
		m.IP = &ip
	}
	return m
}
