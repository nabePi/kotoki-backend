package balance

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/nabePi/kotoki-backend/internal/platform/clock"
	"github.com/nabePi/kotoki-backend/internal/platform/errs"
	"github.com/nabePi/kotoki-backend/internal/platform/idgen"
)

// Service implements balance reads and (from Task 7) admin movements.
type Service struct {
	store Store
	clk   clock.Clock
	ids   idgen.Generator
}

// NewService returns a Service.
func NewService(store Store, clk clock.Clock, ids idgen.Generator) *Service {
	return &Service{store: store, clk: clk, ids: ids}
}

// OpenAccount creates the user's account inside the caller's transaction and,
// when initialSeconds > 0, books the initial GRANT (spec 4.6.6).
func OpenAccount(ctx context.Context, tx Tx, ids idgen.Generator, userID uuid.UUID, initialSeconds int64, at time.Time) error {
	repo := tx.Balance()
	acc := Account{ID: ids.New(), UserID: userID, CreatedAt: at, UpdatedAt: at}
	if err := repo.InsertAccount(ctx, acc); err != nil {
		return err
	}
	if initialSeconds <= 0 {
		return nil
	}
	reason := "initial balance"
	_, err := repo.ApplyMovement(ctx, acc, Movement{
		EntryType:      EntryGrant,
		AvailableDelta: initialSeconds,
		ReferenceType:  RefRegistration,
		Reason:         &reason,
		IdempotencyKey: "grant:initial:" + userID.String(),
		At:             at,
	})
	return err
}

// GetBalance returns the account and the durations it can currently afford.
func (s *Service) GetBalance(ctx context.Context, userID uuid.UUID) (BalanceView, error) {
	var view BalanceView
	err := s.store.WithinTx(ctx, func(ctx context.Context, tx Tx) error {
		acc, err := tx.Balance().GetAccountByUserID(ctx, userID)
		if err != nil {
			return err
		}
		view = BalanceView{Account: acc, AllowedDurations: affordable(acc.AvailableSeconds)}
		return nil
	})
	if errors.Is(err, ErrAccountNotFound) {
		return BalanceView{}, errs.Wrap(err, errs.CodeUserNotFound, "user not found")
	}
	return view, err
}

func affordable(available int64) []int64 {
	out := []int64{}
	for _, d := range AllowedDurations {
		if d <= available {
			out = append(out, d)
		}
	}
	return out
}

// ListTransactions returns up to limit entries newest first plus the cursor of the
// next page (nil on the last page).
func (s *Service) ListTransactions(ctx context.Context, userID uuid.UUID, types []EntryType, after *uuid.UUID, limit int) ([]LedgerEntry, *uuid.UUID, error) {
	var entries []LedgerEntry
	err := s.store.WithinTx(ctx, func(ctx context.Context, tx Tx) error {
		var err error
		entries, err = tx.Balance().ListLedger(ctx, userID, types, after, limit+1)
		return err
	})
	if err != nil {
		return nil, nil, err
	}
	if len(entries) > limit {
		entries = entries[:limit]
		next := entries[limit-1].ID
		return entries, &next, nil
	}
	return entries, nil, nil
}

const (
	maxAdminSeconds = 360000 // 100 hours
	minReasonLen    = 3
	maxReasonLen    = 500
)

// GrantInput is an admin grant request (spec 4.6.6).
type GrantInput struct {
	UserID, AdminUserID uuid.UUID
	Seconds             int64
	Reason              string
	IdempotencyKey      string
	At                  time.Time
}

// AdjustInput is an admin adjustment request (spec 4.6.7).
type AdjustInput struct {
	UserID, AdminUserID uuid.UUID
	SecondsDelta        int64
	Reason              string
	IdempotencyKey      string
	At                  time.Time
}

// MovementResult is the outcome of a grant/adjustment: the ledger entry, the
// account after it, and whether an earlier identical request was replayed.
type MovementResult struct {
	Entry    LedgerEntry
	Account  Account
	Replayed bool
}

// ValidateAdminIdempotencyKey requires a UUID Idempotency-Key.
func ValidateAdminIdempotencyKey(key string) error {
	if key == "" {
		return errs.New(errs.CodeIdempotencyKeyRequired, "Idempotency-Key header is required")
	}
	if _, err := uuid.Parse(key); err != nil {
		return errs.New(errs.CodeIdempotencyKeyInvalid, "Idempotency-Key must be a UUID")
	}
	return nil
}

func validationError(field, reason string) error {
	return errs.WithDetails(errs.New(errs.CodeValidationFailed, "invalid "+field),
		map[string]any{"fields": []map[string]any{{"field": field, "reason": reason}}})
}

func validateReason(reason string) error {
	if n := len([]rune(reason)); n < minReasonLen || n > maxReasonLen {
		return validationError("reason", "length")
	}
	return nil
}

func accountAfter(acc Account, e LedgerEntry) Account {
	acc.AvailableSeconds, acc.ReservedSeconds, acc.Version = e.AvailableSecondsAfter, e.ReservedSecondsAfter, e.AccountVersion
	acc.UpdatedAt = e.CreatedAt
	return acc
}

func (s *Service) lockAccount(ctx context.Context, tx Tx, userID uuid.UUID) (Account, error) {
	acc, err := tx.Balance().LockAccountByUserID(ctx, userID)
	if errors.Is(err, ErrAccountNotFound) {
		return Account{}, errs.Wrap(err, errs.CodeUserNotFound, "user not found")
	}
	return acc, err
}

// GrantTx grants seconds inside the caller's transaction. It locks the account,
// replays an identical earlier request and rejects a reused key.
func (s *Service) GrantTx(ctx context.Context, tx Tx, in GrantInput) (MovementResult, error) {
	if err := ValidateAdminIdempotencyKey(in.IdempotencyKey); err != nil {
		return MovementResult{}, err
	}
	if in.Seconds < 1 || in.Seconds > maxAdminSeconds {
		return MovementResult{}, validationError("seconds", "out_of_range")
	}
	if err := validateReason(in.Reason); err != nil {
		return MovementResult{}, err
	}
	acc, err := s.lockAccount(ctx, tx, in.UserID)
	if err != nil {
		return MovementResult{}, err
	}
	key := "grant:" + in.IdempotencyKey
	if existing, ok, err := tx.Balance().FindLedgerByIdempotencyKey(ctx, key); err != nil {
		return MovementResult{}, err
	} else if ok {
		if existing.UserID == in.UserID && existing.EntryType == EntryGrant && existing.AvailableSecondsDelta == in.Seconds {
			return MovementResult{Entry: existing, Account: acc, Replayed: true}, nil
		}
		return MovementResult{}, errs.New(errs.CodeIdempotencyKeyReused, "idempotency key already used with a different request")
	}
	reason, admin := in.Reason, in.AdminUserID
	entry, err := tx.Balance().ApplyMovement(ctx, acc, Movement{
		EntryType: EntryGrant, AvailableDelta: in.Seconds, ReferenceType: RefAdmin,
		AdminUserID: &admin, Reason: &reason, IdempotencyKey: key, At: in.At,
	})
	if err != nil {
		return MovementResult{}, err
	}
	return MovementResult{Entry: entry, Account: accountAfter(acc, entry)}, nil
}

// AdjustTx adds a signed delta to available seconds inside the caller's transaction.
func (s *Service) AdjustTx(ctx context.Context, tx Tx, in AdjustInput) (MovementResult, error) {
	if err := ValidateAdminIdempotencyKey(in.IdempotencyKey); err != nil {
		return MovementResult{}, err
	}
	if in.SecondsDelta == 0 || in.SecondsDelta > maxAdminSeconds || in.SecondsDelta < -maxAdminSeconds {
		return MovementResult{}, validationError("seconds_delta", "out_of_range")
	}
	if err := validateReason(in.Reason); err != nil {
		return MovementResult{}, err
	}
	acc, err := s.lockAccount(ctx, tx, in.UserID)
	if err != nil {
		return MovementResult{}, err
	}
	key := "adjust:" + in.IdempotencyKey
	if existing, ok, err := tx.Balance().FindLedgerByIdempotencyKey(ctx, key); err != nil {
		return MovementResult{}, err
	} else if ok {
		if existing.UserID == in.UserID && existing.EntryType == EntryAdjustment && existing.AvailableSecondsDelta == in.SecondsDelta {
			return MovementResult{Entry: existing, Account: acc, Replayed: true}, nil
		}
		return MovementResult{}, errs.New(errs.CodeIdempotencyKeyReused, "idempotency key already used with a different request")
	}
	if acc.AvailableSeconds+in.SecondsDelta < 0 {
		return MovementResult{}, errs.WithDetails(
			errs.New(errs.CodeBalanceAdjustmentNegative, "adjustment would make the balance negative"),
			map[string]any{"available_seconds": acc.AvailableSeconds, "seconds_delta": in.SecondsDelta})
	}
	reason, admin := in.Reason, in.AdminUserID
	entry, err := tx.Balance().ApplyMovement(ctx, acc, Movement{
		EntryType: EntryAdjustment, AvailableDelta: in.SecondsDelta, ReferenceType: RefAdmin,
		AdminUserID: &admin, Reason: &reason, IdempotencyKey: key, At: in.At,
	})
	if err != nil {
		return MovementResult{}, err
	}
	return MovementResult{Entry: entry, Account: accountAfter(acc, entry)}, nil
}

// withDuplicateRetry runs op in its own transaction; a concurrent request that
// grabbed the same key makes ApplyMovement fail with ErrDuplicateMovement, and
// the retry then sees the winner's entry and replays or rejects.
func (s *Service) withDuplicateRetry(ctx context.Context, op func(ctx context.Context, tx Tx) (MovementResult, error)) (MovementResult, error) {
	var res MovementResult
	run := func() error {
		return s.store.WithinTx(ctx, func(ctx context.Context, tx Tx) (err error) {
			res, err = op(ctx, tx)
			return err
		})
	}
	err := run()
	if errors.Is(err, ErrDuplicateMovement) {
		err = run()
	}
	return res, err
}

// Grant is the standalone form of GrantTx.
func (s *Service) Grant(ctx context.Context, in GrantInput) (MovementResult, error) {
	return s.withDuplicateRetry(ctx, func(ctx context.Context, tx Tx) (MovementResult, error) { return s.GrantTx(ctx, tx, in) })
}

// Adjust is the standalone form of AdjustTx.
func (s *Service) Adjust(ctx context.Context, in AdjustInput) (MovementResult, error) {
	return s.withDuplicateRetry(ctx, func(ctx context.Context, tx Tx) (MovementResult, error) { return s.AdjustTx(ctx, tx, in) })
}
