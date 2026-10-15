// Package admin implements the admin use cases: balance grants/adjustments with
// audit, user status changes, account deletion and audit browsing (spec 3.12).
package admin

import (
	"context"
	"net/netip"
	"time"

	"github.com/google/uuid"
)

// Audit actions and target types (spec 3.3.3).
const (
	ActionBalanceGrant      = "BALANCE_GRANT"
	ActionBalanceAdjustment = "BALANCE_ADJUSTMENT"
	ActionUserStatusChange  = "USER_STATUS_CHANGE"

	ActionSessionTranscriptView = "SESSION_TRANSCRIPT_VIEW"

	TargetUser    = "USER"
	TargetSession = "SESSION"
)

// AuditLog mirrors admin_audit_logs. It is append-only.
type AuditLog struct {
	ID             uuid.UUID
	AdminUserID    uuid.UUID
	Action         string
	TargetType     string
	TargetID       string
	TargetUserID   *uuid.UUID
	RequestID      string
	IdempotencyKey *string
	Reason         *string
	Details        map[string]any
	IP             *netip.Addr
	UserAgent      *string
	CreatedAt      time.Time
}

// AuditFilter narrows AuditRepository.List; zero fields match everything.
type AuditFilter struct {
	AdminUserID, TargetUserID *uuid.UUID
	Action                    string
}

// AuditRepository persists audit rows.
type AuditRepository interface {
	Insert(ctx context.Context, a AuditLog) error
	List(ctx context.Context, f AuditFilter, after *uuid.UUID, limit int) ([]AuditLog, error) // id DESC
}

// RequestMeta is the request context recorded with every audit row.
type RequestMeta struct {
	RequestID string
	IP        *netip.Addr
	UserAgent string
}

// OpenSession is a session that still holds a reservation or runtime.
type OpenSession struct {
	ID     uuid.UUID
	Status string
}

// SessionGate is the narrow window onto sessions that account deletion needs
// (session.Service arrives in M3).
type SessionGate interface {
	// OpenSessions lists the user's sessions in RESERVED/STARTING/ACTIVE/ENDING.
	OpenSessions(ctx context.Context, userID uuid.UUID) ([]OpenSession, error)
	// CancelReserved cancels a RESERVED session and releases its reservation inside the caller's tx.
	CancelReserved(ctx context.Context, sessionID uuid.UUID, at time.Time) error
}
