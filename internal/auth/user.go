// Package auth owns identity: users, passwords, tokens, tickets and the ports
// they need. It never imports infrastructure (pgx, chi, go-redis).
package auth

import (
	"context"
	"net/netip"
	"time"

	"github.com/google/uuid"
)

// Role is the user's authorization role (D-023).
type Role string

// Roles.
const (
	RoleUser  Role = "USER"
	RoleAdmin Role = "ADMIN"
)

// UserStatus is the account status (spec 3.3.1).
type UserStatus string

// User statuses.
const (
	UserStatusActive    UserStatus = "ACTIVE"
	UserStatusSuspended UserStatus = "SUSPENDED"
	UserStatusDeleted   UserStatus = "DELETED"
)

// User mirrors the users table (spec 3.3.1).
type User struct {
	ID                uuid.UUID
	Email             string
	PhoneNumber       *string
	FullName          string
	PasswordHash      string
	Role              Role
	Status            UserStatus
	PasswordChangedAt time.Time
	LastLoginAt       *time.Time
	TermsVersion      string
	TermsAcceptedAt   time.Time
	CreatedAt         time.Time
	UpdatedAt         time.Time
	DeletedAt         *time.Time
	PurgedAt          *time.Time
}

// UserFilter narrows UserRepository.List.
type UserFilter struct {
	EmailPrefix string
	Status      *UserStatus
	Role        *Role
}

// UserRepository persists users.
type UserRepository interface {
	Insert(ctx context.Context, u User) error                // ErrEmailTaken | ErrPhoneTaken
	GetByID(ctx context.Context, id uuid.UUID) (User, error) // ErrUserNotFound
	GetByEmail(ctx context.Context, email string) (User, error)
	LockByID(ctx context.Context, id uuid.UUID) (User, error) // FOR UPDATE
	UpdateLastLogin(ctx context.Context, id uuid.UUID, at time.Time) error
	// SetPasswordHash updates the hash; a nil changedAt leaves password_changed_at untouched.
	SetPasswordHash(ctx context.Context, id uuid.UUID, hash string, changedAt *time.Time) error
	UpdateConsents(ctx context.Context, id uuid.UUID, termsVersion string, at time.Time) (User, error) // ErrUserNotActive
	UpdateStatus(ctx context.Context, id uuid.UUID, to UserStatus, at time.Time) error                 // DELETED fills deleted_at
	List(ctx context.Context, f UserFilter, after *uuid.UUID, limit int) ([]User, error)               // id DESC
}

// RefreshToken mirrors the refresh_tokens table (spec 3.3.2).
type RefreshToken struct {
	ID, UserID, FamilyID uuid.UUID
	ParentID             *uuid.UUID
	TokenHash            []byte
	UserAgent            *string
	IP                   *netip.Addr
	CreatedAt, ExpiresAt time.Time
	RevokedAt            *time.Time
	RevokeReason         *string
	ReplacedByID         *uuid.UUID
}

// RefreshTokenRepository persists refresh tokens.
type RefreshTokenRepository interface {
	Insert(ctx context.Context, t RefreshToken) error
	GetByID(ctx context.Context, id uuid.UUID) (RefreshToken, error)
	LockByHash(ctx context.Context, hash []byte) (RefreshToken, error) // FOR UPDATE; ErrRefreshTokenNotFound
	MarkRotated(ctx context.Context, id, replacedBy uuid.UUID, at time.Time) error
	RevokeFamily(ctx context.Context, familyID uuid.UUID, reason string, at time.Time) (int64, error)
	RevokeAllForUser(ctx context.Context, userID uuid.UUID, reason string, at time.Time) (int64, error)
	DeleteAllForUser(ctx context.Context, userID uuid.UUID) error
	CountActive(ctx context.Context, userID uuid.UUID, now time.Time) (int, error)
}

// PasswordResetToken mirrors the password_reset_tokens table (spec 3.3.2).
type PasswordResetToken struct {
	ID, UserID            uuid.UUID
	TokenHash             []byte
	RequestedIP           *netip.Addr
	CreatedAt, ExpiresAt  time.Time
	UsedAt, InvalidatedAt *time.Time
}

// ResetLookup is a reset token joined with its owner.
type ResetLookup struct {
	Token      PasswordResetToken
	Email      string
	UserStatus UserStatus
}

// PasswordResetRepository persists password reset tokens.
type PasswordResetRepository interface {
	InvalidateOpenForUser(ctx context.Context, userID uuid.UUID, at time.Time) error
	Insert(ctx context.Context, t PasswordResetToken) error
	FindByHash(ctx context.Context, hash []byte) (ResetLookup, error) // no lock; ErrResetTokenNotFound
	LockByHash(ctx context.Context, hash []byte) (ResetLookup, error) // FOR UPDATE OF t
	MarkUsed(ctx context.Context, id uuid.UUID, at time.Time) error
	InvalidateOthers(ctx context.Context, userID, exceptID uuid.UUID, at time.Time) error
}
