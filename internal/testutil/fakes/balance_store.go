package fakes

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/nabePi/kotoki-backend/internal/balance"
	"github.com/nabePi/kotoki-backend/internal/platform/idgen"
)

type balanceState struct {
	accounts     map[uuid.UUID]balance.Account     // by user id
	reservations map[uuid.UUID]balance.Reservation // by session id
	ledger       []balance.LedgerEntry
}

func (s balanceState) clone() balanceState {
	c := balanceState{
		accounts: make(map[uuid.UUID]balance.Account, len(s.accounts)), reservations: make(map[uuid.UUID]balance.Reservation, len(s.reservations)),
		ledger: slices.Clone(s.ledger),
	}
	for k, v := range s.accounts {
		c.accounts[k] = v
	}
	for k, v := range s.reservations {
		c.reservations[k] = cloneReservation(v)
	}
	return c
}

func cloneReservation(r balance.Reservation) balance.Reservation {
	cp := func(p *int64) *int64 {
		if p == nil {
			return nil
		}
		v := *p
		return &v
	}
	r.ConsumedSeconds, r.ReleasedSeconds = cp(r.ConsumedSeconds), cp(r.ReleasedSeconds)
	return r
}

// BalanceStore is an in-memory balance.Store. WithinTx serializes callers and
// restores the previous state when fn fails, mimicking a rollback.
type BalanceStore struct {
	FailInjector
	Rec *CallRecorder
	mu  sync.Mutex
	ids idgen.Generator
	st  balanceState
}

var _ balance.Store = (*BalanceStore)(nil)

// BalanceOption customises NewBalanceStore.
type BalanceOption func(*BalanceStore)

// WithBalanceIDs sets the generator used for ledger entry ids.
func WithBalanceIDs(g idgen.Generator) BalanceOption { return func(s *BalanceStore) { s.ids = g } }

// NewBalanceStore returns an empty store.
func NewBalanceStore(opts ...BalanceOption) *BalanceStore {
	s := &BalanceStore{ids: idgen.NewV7(), st: balanceState{accounts: map[uuid.UUID]balance.Account{}, reservations: map[uuid.UUID]balance.Reservation{}}}
	for _, o := range opts {
		o(s)
	}
	return s
}

// WithinTx runs fn on the live state and rolls it back when fn returns an error.
func (s *BalanceStore) WithinTx(ctx context.Context, fn func(ctx context.Context, tx balance.Tx) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	snapshot := s.st.clone()
	if err := fn(ctx, &balanceTx{s: s}); err != nil {
		s.st = snapshot
		return err
	}
	return nil
}

// Account returns the stored account of userID (zero value if none).
func (s *BalanceStore) Account(userID uuid.UUID) balance.Account {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.st.accounts[userID]
}

// Ledger returns every ledger entry in insertion order.
func (s *BalanceStore) Ledger() []balance.LedgerEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.st.ledger)
}

// Reservation returns the reservation of sessionID (zero value if none).
func (s *BalanceStore) Reservation(sessionID uuid.UUID) balance.Reservation {
	s.mu.Lock()
	defer s.mu.Unlock()
	return cloneReservation(s.st.reservations[sessionID])
}

type balanceTx struct{ s *BalanceStore }

func (t *balanceTx) Balance() balance.Repository { return &balanceRepo{s: t.s} }

type balanceRepo struct{ s *BalanceStore }

func (r *balanceRepo) account(userID uuid.UUID) (balance.Account, error) {
	a, ok := r.s.st.accounts[userID]
	if !ok {
		return balance.Account{}, balance.ErrAccountNotFound
	}
	return a, nil
}

func (r *balanceRepo) LockAccountByUserID(_ context.Context, userID uuid.UUID) (balance.Account, error) {
	r.s.Rec.Record("Balance.LockAccountByUserID")
	if err := r.s.Take("LockAccountByUserID"); err != nil {
		return balance.Account{}, err
	}
	return r.account(userID)
}

func (r *balanceRepo) GetAccountByUserID(_ context.Context, userID uuid.UUID) (balance.Account, error) {
	if err := r.s.Take("GetAccountByUserID"); err != nil {
		return balance.Account{}, err
	}
	return r.account(userID)
}

func (r *balanceRepo) InsertAccount(_ context.Context, a balance.Account) error {
	if err := r.s.Take("InsertAccount"); err != nil {
		return err
	}
	if _, ok := r.s.st.accounts[a.UserID]; ok {
		return errors.New("fakes: duplicate balance account for user")
	}
	r.s.st.accounts[a.UserID] = a
	return nil
}

func (r *balanceRepo) ApplyMovement(_ context.Context, acc balance.Account, m balance.Movement) (balance.LedgerEntry, error) {
	if err := r.s.Take("ApplyMovement"); err != nil {
		return balance.LedgerEntry{}, err
	}
	cur, err := r.account(acc.UserID)
	if err != nil {
		return balance.LedgerEntry{}, err
	}
	for _, e := range r.s.st.ledger {
		if e.IdempotencyKey == m.IdempotencyKey {
			return balance.LedgerEntry{}, balance.ErrDuplicateMovement
		}
	}
	cur.AvailableSeconds += m.AvailableDelta
	cur.ReservedSeconds += m.ReservedDelta
	if cur.AvailableSeconds < 0 || cur.ReservedSeconds < 0 {
		return balance.LedgerEntry{}, errors.New("fakes: movement would make a balance negative")
	}
	cur.Version++
	cur.UpdatedAt = m.At
	r.s.st.accounts[acc.UserID] = cur
	e := balance.LedgerEntry{
		ID: r.s.ids.New(), UserID: cur.UserID, AccountID: cur.ID, EntryType: m.EntryType,
		AvailableSecondsDelta: m.AvailableDelta, ReservedSecondsDelta: m.ReservedDelta,
		AvailableSecondsAfter: cur.AvailableSeconds, ReservedSecondsAfter: cur.ReservedSeconds, AccountVersion: cur.Version,
		SessionID: m.SessionID, ReservationID: m.ReservationID, ReferenceType: m.ReferenceType,
		AdminUserID: m.AdminUserID, Reason: m.Reason, IdempotencyKey: m.IdempotencyKey, CreatedAt: m.At,
	}
	r.s.st.ledger = append(r.s.st.ledger, e)
	return e, nil
}

func (r *balanceRepo) FindLedgerByIdempotencyKey(_ context.Context, key string) (balance.LedgerEntry, bool, error) {
	if err := r.s.Take("FindLedgerByIdempotencyKey"); err != nil {
		return balance.LedgerEntry{}, false, err
	}
	for _, e := range r.s.st.ledger {
		if e.IdempotencyKey == key {
			return e, true, nil
		}
	}
	return balance.LedgerEntry{}, false, nil
}

func (r *balanceRepo) ListLedger(_ context.Context, userID uuid.UUID, types []balance.EntryType, after *uuid.UUID, limit int) ([]balance.LedgerEntry, error) {
	if err := r.s.Take("ListLedger"); err != nil {
		return nil, err
	}
	var out []balance.LedgerEntry
	for _, e := range r.s.st.ledger {
		if e.UserID != userID || (len(types) > 0 && !slices.Contains(types, e.EntryType)) {
			continue
		}
		if after != nil && bytes.Compare(e.ID[:], after[:]) >= 0 {
			continue
		}
		out = append(out, e)
	}
	slices.SortFunc(out, func(a, b balance.LedgerEntry) int { return bytes.Compare(b.ID[:], a.ID[:]) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (r *balanceRepo) LockReservationBySessionID(_ context.Context, sessionID uuid.UUID) (balance.Reservation, error) {
	r.s.Rec.Record("Balance.LockReservationBySessionID")
	if err := r.s.Take("LockReservationBySessionID"); err != nil {
		return balance.Reservation{}, err
	}
	res, ok := r.s.st.reservations[sessionID]
	if !ok {
		return balance.Reservation{}, balance.ErrReservationNotFound
	}
	return cloneReservation(res), nil
}

func (r *balanceRepo) InsertReservation(_ context.Context, res balance.Reservation) error {
	if err := r.s.Take("InsertReservation"); err != nil {
		return err
	}
	if _, dup := r.s.st.reservations[res.SessionID]; dup {
		return errors.New("fakes: duplicate reservation for session (ux_balance_reservations_session)")
	}
	if res.Status != balance.ReservationHeld || res.ConsumedSeconds != nil || res.ReleasedSeconds != nil || res.SettledAt != nil || res.ReleasedAt != nil {
		return errors.New("fakes: a new reservation must be HELD without settlement fields (chk_reservation_state)")
	}
	if !res.StartDeadlineAt.After(res.CreatedAt) {
		return errors.New("fakes: start_deadline_at must be after created_at (chk_reservation_deadline)")
	}
	r.s.st.reservations[res.SessionID] = cloneReservation(res)
	return nil
}

func (r *balanceRepo) byID(id uuid.UUID) (balance.Reservation, bool) {
	for _, res := range r.s.st.reservations {
		if res.ID == id {
			return res, true
		}
	}
	return balance.Reservation{}, false
}

func (r *balanceRepo) MarkReservationSettled(_ context.Context, id uuid.UUID, consumed, released int64, at time.Time) error {
	if err := r.s.Take("MarkReservationSettled"); err != nil {
		return err
	}
	res, ok := r.byID(id)
	if !ok {
		return balance.ErrReservationNotFound
	}
	if consumed < 0 || consumed > res.ReservedSeconds || released != res.ReservedSeconds-consumed {
		return errors.New("fakes: SETTLED needs consumed in [0, reserved] and released = reserved − consumed (chk_reservation_state)")
	}
	res.Status, res.ConsumedSeconds, res.ReleasedSeconds, res.SettledAt = balance.ReservationSettled, &consumed, &released, &at
	r.s.st.reservations[res.SessionID] = res
	return nil
}

func (r *balanceRepo) MarkReservationReleased(_ context.Context, id uuid.UUID, at time.Time) error {
	if err := r.s.Take("MarkReservationReleased"); err != nil {
		return err
	}
	res, ok := r.byID(id)
	if !ok {
		return balance.ErrReservationNotFound
	}
	zero, all := int64(0), res.ReservedSeconds
	res.Status, res.ConsumedSeconds, res.ReleasedSeconds, res.ReleasedAt = balance.ReservationReleased, &zero, &all, &at
	r.s.st.reservations[res.SessionID] = res
	return nil
}
