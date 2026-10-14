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
