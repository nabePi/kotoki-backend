package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/nabePi/kotoki-backend/internal/balance"
	"github.com/nabePi/kotoki-backend/internal/platform/errs"
	"github.com/nabePi/kotoki-backend/internal/platform/idgen"
	"github.com/nabePi/kotoki-backend/internal/postgres/sqlcgen"
)

const uniqueViolation = "23505"

// balanceRepo implements balance.Repository on a transaction.
type balanceRepo struct {
	q   *sqlcgen.Queries
	ids idgen.Generator
}

var _ balance.Repository = (*balanceRepo)(nil)

// Balance returns the transaction's balance repository.
func (t *Tx) Balance() balance.Repository { return &balanceRepo{q: t.Queries(), ids: t.IDs()} }

func toAccount(a sqlcgen.BalanceAccount) balance.Account {
	return balance.Account{
		ID: a.ID, UserID: a.UserID, AvailableSeconds: a.AvailableSeconds, ReservedSeconds: a.ReservedSeconds,
		Version: a.Version, CreatedAt: fromTsz(a.CreatedAt), UpdatedAt: fromTsz(a.UpdatedAt),
	}
}

func toLedger(l sqlcgen.BalanceLedger) balance.LedgerEntry {
	return balance.LedgerEntry{
		ID: l.ID, UserID: l.UserID, AccountID: l.AccountID, EntryType: balance.EntryType(l.EntryType),
		AvailableSecondsDelta: l.AvailableSecondsDelta, ReservedSecondsDelta: l.ReservedSecondsDelta,
		AvailableSecondsAfter: l.AvailableSecondsAfter, ReservedSecondsAfter: l.ReservedSecondsAfter, AccountVersion: l.AccountVersion,
		SessionID: fromNullUUID(l.SessionID), ReservationID: fromNullUUID(l.ReservationID),
		ReferenceType: balance.ReferenceType(l.ReferenceType), AdminUserID: fromNullUUID(l.AdminUserID),
		Reason: fromText(l.Reason), IdempotencyKey: l.IdempotencyKey, CreatedAt: fromTsz(l.CreatedAt),
	}
}

func internalErr(err error, msg string) error { return errs.Wrap(err, errs.CodeInternalError, msg) }

func (r *balanceRepo) LockAccountByUserID(ctx context.Context, userID uuid.UUID) (balance.Account, error) {
	a, err := r.q.LockBalanceAccountByUser(ctx, userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return balance.Account{}, balance.ErrAccountNotFound
	}
	if err != nil {
		return balance.Account{}, internalErr(err, "lock balance account failed")
	}
	return toAccount(a), nil
}

func (r *balanceRepo) GetAccountByUserID(ctx context.Context, userID uuid.UUID) (balance.Account, error) {
	a, err := r.q.GetBalanceAccountByUser(ctx, userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return balance.Account{}, balance.ErrAccountNotFound
	}
	if err != nil {
		return balance.Account{}, internalErr(err, "read balance account failed")
	}
	return toAccount(a), nil
}

func (r *balanceRepo) InsertAccount(ctx context.Context, a balance.Account) error {
	err := r.q.InsertBalanceAccount(ctx, sqlcgen.InsertBalanceAccountParams{
		ID: a.ID, UserID: a.UserID, AvailableSeconds: a.AvailableSeconds, ReservedSeconds: a.ReservedSeconds,
		Version: a.Version, CreatedAt: tsz(a.CreatedAt), UpdatedAt: tsz(a.UpdatedAt),
	})
	if err != nil {
		return internalErr(err, "insert balance account failed")
	}
	return nil
}

// ApplyMovement updates the locked account, then books the ledger entry with the
// resulting balance. A duplicate idempotency key surfaces as ErrDuplicateMovement
// (the caller's transaction is rolled back, undoing the account update).
func (r *balanceRepo) ApplyMovement(ctx context.Context, acc balance.Account, m balance.Movement) (balance.LedgerEntry, error) {
	updated, err := r.q.ApplyAccountDelta(ctx, sqlcgen.ApplyAccountDeltaParams{
		AvailableDelta: m.AvailableDelta, ReservedDelta: m.ReservedDelta, At: tsz(m.At), ID: acc.ID,
	})
	if err != nil {
		return balance.LedgerEntry{}, internalErr(err, "apply balance delta failed")
	}
	entry := sqlcgen.InsertLedgerEntryParams{
		ID: r.ids.New(), UserID: updated.UserID, AccountID: updated.ID, EntryType: string(m.EntryType),
		AvailableSecondsDelta: m.AvailableDelta, ReservedSecondsDelta: m.ReservedDelta,
		AvailableSecondsAfter: updated.AvailableSeconds, ReservedSecondsAfter: updated.ReservedSeconds, AccountVersion: updated.Version,
		SessionID: nullUUID(m.SessionID), ReservationID: nullUUID(m.ReservationID), ReferenceType: string(m.ReferenceType),
		AdminUserID: nullUUID(m.AdminUserID), Reason: textPtr(m.Reason), IdempotencyKey: m.IdempotencyKey, CreatedAt: tsz(m.At),
	}
	if err := r.q.InsertLedgerEntry(ctx, entry); err != nil {
		var pe *pgconn.PgError
		if errors.As(err, &pe) && pe.Code == uniqueViolation && pe.ConstraintName == "ux_balance_ledger_idempotency" {
			return balance.LedgerEntry{}, balance.ErrDuplicateMovement
		}
		return balance.LedgerEntry{}, internalErr(err, "insert ledger entry failed")
	}
	return balance.LedgerEntry{
		ID: entry.ID, UserID: entry.UserID, AccountID: entry.AccountID, EntryType: m.EntryType,
		AvailableSecondsDelta: m.AvailableDelta, ReservedSecondsDelta: m.ReservedDelta,
		AvailableSecondsAfter: entry.AvailableSecondsAfter, ReservedSecondsAfter: entry.ReservedSecondsAfter, AccountVersion: entry.AccountVersion,
		SessionID: m.SessionID, ReservationID: m.ReservationID, ReferenceType: m.ReferenceType, AdminUserID: m.AdminUserID,
		Reason: m.Reason, IdempotencyKey: m.IdempotencyKey, CreatedAt: m.At.UTC(),
	}, nil
}

func (r *balanceRepo) FindLedgerByIdempotencyKey(ctx context.Context, key string) (balance.LedgerEntry, bool, error) {
	l, err := r.q.FindLedgerByKey(ctx, key)
	if errors.Is(err, pgx.ErrNoRows) {
		return balance.LedgerEntry{}, false, nil
	}
	if err != nil {
		return balance.LedgerEntry{}, false, internalErr(err, "find ledger entry failed")
	}
	return toLedger(l), true, nil
}

func (r *balanceRepo) ListLedger(ctx context.Context, userID uuid.UUID, types []balance.EntryType, after *uuid.UUID, limit int) ([]balance.LedgerEntry, error) {
	names := make([]string, len(types))
	for i, t := range types {
		names[i] = string(t)
	}
	rows, err := r.q.ListLedgerForUser(ctx, sqlcgen.ListLedgerForUserParams{
		UserID: userID, Types: names, After: nullUUID(after), RowLimit: int32(min(limit, 1<<30)), //nolint:gosec // bounded above
	})
	if err != nil {
		return nil, internalErr(err, "list ledger failed")
	}
	out := make([]balance.LedgerEntry, len(rows))
	for i, l := range rows {
		out[i] = toLedger(l)
	}
	return out, nil
}

func toReservation(r sqlcgen.BalanceReservation) balance.Reservation {
	i64 := func(v pgtype.Int8) *int64 {
		if !v.Valid {
			return nil
		}
		x := v.Int64
		return &x
	}
	return balance.Reservation{
		ID: r.ID, UserID: r.UserID, AccountID: r.AccountID, SessionID: r.SessionID, ReservedSeconds: r.ReservedSeconds,
		ConsumedSeconds: i64(r.ConsumedSeconds), ReleasedSeconds: i64(r.ReleasedSeconds), Status: balance.ReservationStatus(r.Status),
		StartDeadlineAt: fromTsz(r.StartDeadlineAt), CreatedAt: fromTsz(r.CreatedAt),
		SettledAt: fromTszPtr(r.SettledAt), ReleasedAt: fromTszPtr(r.ReleasedAt),
	}
}

func (r *balanceRepo) LockReservationBySessionID(ctx context.Context, sessionID uuid.UUID) (balance.Reservation, error) {
	res, err := r.q.LockReservationBySession(ctx, sessionID)
	if errors.Is(err, pgx.ErrNoRows) {
		return balance.Reservation{}, balance.ErrReservationNotFound
	}
	if err != nil {
		return balance.Reservation{}, internalErr(err, "lock reservation failed")
	}
	return toReservation(res), nil
}

func (r *balanceRepo) InsertReservation(ctx context.Context, res balance.Reservation) error {
	err := r.q.InsertReservation(ctx, sqlcgen.InsertReservationParams{
		ID: res.ID, UserID: res.UserID, AccountID: res.AccountID, SessionID: res.SessionID, ReservedSeconds: res.ReservedSeconds,
		StartDeadlineAt: tsz(res.StartDeadlineAt), CreatedAt: tsz(res.CreatedAt),
	})
	if err != nil {
		return internalErr(err, "insert reservation failed")
	}
	return nil
}

// MarkReservationSettled and MarkReservationReleased only move a HELD reservation; zero rows
// means it was not HELD (callers lock and check first, so this is a bug guard).
func (r *balanceRepo) MarkReservationSettled(ctx context.Context, id uuid.UUID, consumed, released int64, at time.Time) error {
	n, err := r.q.MarkReservationSettled(ctx, sqlcgen.MarkReservationSettledParams{Consumed: consumed, Released: released, At: tsz(at), ID: id})
	if err != nil {
		return internalErr(err, "settle reservation failed")
	}
	if n != 1 {
		return balance.ErrReservationState
	}
	return nil
}

func (r *balanceRepo) MarkReservationReleased(ctx context.Context, id uuid.UUID, at time.Time) error {
	n, err := r.q.MarkReservationReleased(ctx, sqlcgen.MarkReservationReleasedParams{At: tsz(at), ID: id})
	if err != nil {
		return internalErr(err, "release reservation failed")
	}
	if n != 1 {
		return balance.ErrReservationState
	}
	return nil
}
