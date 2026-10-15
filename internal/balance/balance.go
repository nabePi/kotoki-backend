// Package balance owns the account, reservation and ledger rules (spec Bab 4).
// Only this package changes balance_accounts, balance_reservations and balance_ledger.
package balance

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
)

// EntryType is the kind of ledger entry (spec 4.3.3).
type EntryType string

// Entry types.
const (
	EntryGrant      EntryType = "GRANT"
	EntryAdjustment EntryType = "ADJUSTMENT"
	EntryReserve    EntryType = "RESERVE"
	EntryRelease    EntryType = "RELEASE"
	EntryConsume    EntryType = "CONSUME"
)

// Valid reports whether t is a known entry type.
func (t EntryType) Valid() bool {
	switch t {
	case EntryGrant, EntryAdjustment, EntryReserve, EntryRelease, EntryConsume:
		return true
	}
	return false
}

// ReferenceType says what caused a ledger entry.
type ReferenceType string

// Reference types.
const (
	RefSession      ReferenceType = "SESSION"
	RefAdmin        ReferenceType = "ADMIN"
	RefRegistration ReferenceType = "REGISTRATION"
)

// Account mirrors balance_accounts (spec 4.3.1). Seconds are non-negative.
type Account struct {
	ID, UserID                                 uuid.UUID
	AvailableSeconds, ReservedSeconds, Version int64
	CreatedAt, UpdatedAt                       time.Time
}

// LedgerEntry mirrors balance_ledger (spec 4.3.3).
type LedgerEntry struct {
	ID, UserID, AccountID                                       uuid.UUID
	EntryType                                                   EntryType
	AvailableSecondsDelta, ReservedSecondsDelta                 int64
	AvailableSecondsAfter, ReservedSecondsAfter, AccountVersion int64
	SessionID, ReservationID                                    *uuid.UUID
	ReferenceType                                               ReferenceType
	AdminUserID                                                 *uuid.UUID
	Reason                                                      *string
	IdempotencyKey                                              string
	CreatedAt                                                   time.Time
}

// Movement is one balance change applied by ApplyMovement (spec 4.2).
type Movement struct {
	EntryType      EntryType
	AvailableDelta int64
	ReservedDelta  int64
	SessionID      *uuid.UUID
	ReservationID  *uuid.UUID
	ReferenceType  ReferenceType
	AdminUserID    *uuid.UUID
	Reason         *string
	IdempotencyKey string
	At             time.Time
}

// Sentinel errors of the repository port.
var (
	ErrAccountNotFound     = errors.New("balance: account not found")
	ErrDuplicateMovement   = errors.New("balance: duplicate idempotency key")
	ErrReservationNotFound = errors.New("balance: reservation not found")
	// ErrReservationState means a reservation is not HELD where it must be; callers log it as
	// reservation_state_mismatch and answer INTERNAL_ERROR (spec 4.6.2/4.6.3).
	ErrReservationState = errors.New("balance: reservation state mismatch")
)

// ReservationStatus is the lifecycle of a reservation (spec 4.3.2).
type ReservationStatus string

// Reservation statuses.
const (
	ReservationHeld     ReservationStatus = "HELD"
	ReservationSettled  ReservationStatus = "SETTLED"
	ReservationReleased ReservationStatus = "RELEASED"
)

// Reservation mirrors balance_reservations (spec 4.3.2): the seconds held for one session.
type Reservation struct {
	ID, UserID, AccountID, SessionID uuid.UUID
	ReservedSeconds                  int64
	ConsumedSeconds, ReleasedSeconds *int64
	Status                           ReservationStatus
	StartDeadlineAt, CreatedAt       time.Time
	SettledAt, ReleasedAt            *time.Time
}

// ReservationRepository is the reservation part of the persistence port. Callers hold the
// account lock first (lock order, spec 4.6.0).
type ReservationRepository interface {
	// LockReservationBySessionID is SELECT … FOR UPDATE; ErrReservationNotFound when absent.
	LockReservationBySessionID(ctx context.Context, sessionID uuid.UUID) (Reservation, error)
	InsertReservation(ctx context.Context, r Reservation) error
	// MarkReservationSettled sets SETTLED with consumed+released = reserved (chk_reservation_state).
	MarkReservationSettled(ctx context.Context, id uuid.UUID, consumed, released int64, at time.Time) error
	// MarkReservationReleased sets RELEASED with consumed = 0 and released = reserved.
	MarkReservationReleased(ctx context.Context, id uuid.UUID, at time.Time) error
}

// Repository is the persistence port for accounts and the ledger. M3 extends it
// with reservation methods (spec 4.2).
type Repository interface {
	ReservationRepository
	LockAccountByUserID(ctx context.Context, userID uuid.UUID) (Account, error) // SELECT … FOR UPDATE; ErrAccountNotFound
	GetAccountByUserID(ctx context.Context, userID uuid.UUID) (Account, error)  // no lock; ErrAccountNotFound
	InsertAccount(ctx context.Context, a Account) error
	// ApplyMovement is the only way to change a balance: it updates the (already
	// locked) account and inserts the ledger entry with *_after = the new balance.
	// A duplicate idempotency key returns ErrDuplicateMovement.
	ApplyMovement(ctx context.Context, acc Account, m Movement) (LedgerEntry, error)
	FindLedgerByIdempotencyKey(ctx context.Context, key string) (LedgerEntry, bool, error)
	// ListLedger returns entries newest first (id DESC); types nil means all.
	ListLedger(ctx context.Context, userID uuid.UUID, types []EntryType, after *uuid.UUID, limit int) ([]LedgerEntry, error)
}

// AllowedDurations are the session durations a user can book, in seconds.
var AllowedDurations = []int64{300, 600, 900, 1800, 2700, 3600}

// BalanceView is the GET /balance payload: the account plus the durations it can afford.
type BalanceView struct { //nolint:revive // name fixed by the plan/spec (§4.7); M3 and httpapi refer to balance.BalanceView
	Account
	AllowedDurations []int64
}

// Description is the server-generated, user-facing text of an entry (spec 4.7).
func Description(e LedgerEntry) string {
	switch {
	case e.EntryType == EntryGrant && e.ReferenceType == RefRegistration:
		return "Welcome balance"
	case e.EntryType == EntryGrant:
		return "Balance granted"
	case e.EntryType == EntryAdjustment:
		return "Balance adjustment"
	case e.EntryType == EntryReserve:
		return "Reserved for session"
	case e.EntryType == EntryConsume:
		return "Used in session"
	case e.EntryType == EntryRelease:
		return "Returned from session"
	}
	return ""
}
