package fakes

import (
	"bytes"
	"context"
	"errors"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/nabePi/kotoki-backend/internal/auth"
	"github.com/nabePi/kotoki-backend/internal/balance"
	"github.com/nabePi/kotoki-backend/internal/platform/idgen"
)

type authState struct {
	users   map[uuid.UUID]auth.User
	refresh map[uuid.UUID]auth.RefreshToken
	resets  map[uuid.UUID]auth.PasswordResetToken
}

func (s authState) clone() authState {
	return authState{users: maps.Clone(s.users), refresh: maps.Clone(s.refresh), resets: maps.Clone(s.resets)}
}

// AuthStore is an in-memory auth.Store. Its transactions nest the shared
// BalanceStore transaction, so a failed callback rolls back both.
type AuthStore struct {
	FailInjector
	Rec *CallRecorder
	mu  sync.Mutex
	bal *BalanceStore
	ids idgen.Generator
	st  authState
}

var _ auth.Store = (*AuthStore)(nil)

// NewAuthStore returns an empty store sharing bal for account opening.
func NewAuthStore(bal *BalanceStore, ids idgen.Generator) *AuthStore {
	return &AuthStore{bal: bal, ids: ids, st: authState{
		users: map[uuid.UUID]auth.User{}, refresh: map[uuid.UUID]auth.RefreshToken{}, resets: map[uuid.UUID]auth.PasswordResetToken{},
	}}
}

// WithinTx runs fn and restores all state when it fails.
func (s *AuthStore) WithinTx(ctx context.Context, fn func(ctx context.Context, tx auth.Tx) error) error {
	return s.within(ctx, func(ctx context.Context, tx *authTx) error { return fn(ctx, tx) })
}

func (s *AuthStore) within(ctx context.Context, fn func(ctx context.Context, tx *authTx) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	snapshot := s.st.clone()
	err := s.bal.WithinTx(ctx, func(ctx context.Context, btx balance.Tx) error {
		return fn(ctx, &authTx{s: s, btx: btx})
	})
	if err != nil {
		s.st = snapshot
	}
	return err
}

// User returns the stored user (zero value if absent).
func (s *AuthStore) User(id uuid.UUID) auth.User {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.st.users[id]
}

// RefreshTokens returns every stored refresh token.
func (s *AuthStore) RefreshTokens() []auth.RefreshToken {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Collect(maps.Values(s.st.refresh))
}

type authTx struct {
	s   *AuthStore
	btx balance.Tx
}

func (t *authTx) Users() auth.UserRepository                   { return &userRepo{s: t.s} }
func (t *authTx) RefreshTokens() auth.RefreshTokenRepository   { return &refreshRepo{s: t.s} }
func (t *authTx) PasswordResets() auth.PasswordResetRepository { return &resetRepo{s: t.s} }
func (t *authTx) Accounts() auth.AccountOpener                 { return &accountOpener{s: t.s, btx: t.btx} }

type accountOpener struct {
	s   *AuthStore
	btx balance.Tx
}

func (a *accountOpener) OpenAccount(ctx context.Context, userID uuid.UUID, initialSeconds int64, at time.Time) error {
	if err := a.s.Take("OpenAccount"); err != nil {
		return err
	}
	return balance.OpenAccount(ctx, a.btx, a.s.ids, userID, initialSeconds, at)
}

// ---- users ----

type userRepo struct{ s *AuthStore }

func (r *userRepo) byEmail(email string) (auth.User, bool) {
	for _, u := range r.s.st.users {
		if strings.EqualFold(u.Email, email) {
			return u, true
		}
	}
	return auth.User{}, false
}

func (r *userRepo) Insert(_ context.Context, u auth.User) error {
	if err := r.s.Take("Users.Insert"); err != nil {
		return err
	}
	if _, ok := r.byEmail(u.Email); ok {
		return auth.ErrEmailTaken
	}
	if u.PhoneNumber != nil {
		for _, o := range r.s.st.users {
			if o.PhoneNumber != nil && *o.PhoneNumber == *u.PhoneNumber {
				return auth.ErrPhoneTaken
			}
		}
	}
	r.s.st.users[u.ID] = u
	return nil
}

func (r *userRepo) GetByID(_ context.Context, id uuid.UUID) (auth.User, error) {
	if err := r.s.Take("Users.GetByID"); err != nil {
		return auth.User{}, err
	}
	u, ok := r.s.st.users[id]
	if !ok {
		return auth.User{}, auth.ErrUserNotFound
	}
	return u, nil
}

func (r *userRepo) GetByEmail(_ context.Context, email string) (auth.User, error) {
	if err := r.s.Take("Users.GetByEmail"); err != nil {
		return auth.User{}, err
	}
	u, ok := r.byEmail(email)
	if !ok {
		return auth.User{}, auth.ErrUserNotFound
	}
	return u, nil
}

func (r *userRepo) LockByID(ctx context.Context, id uuid.UUID) (auth.User, error) {
	r.s.Rec.Record("Users.LockByID")
	return r.GetByID(ctx, id)
}

func (r *userRepo) update(id uuid.UUID, mutate func(*auth.User) error) (auth.User, error) {
	u, ok := r.s.st.users[id]
	if !ok {
		return auth.User{}, auth.ErrUserNotFound
	}
	if err := mutate(&u); err != nil {
		return auth.User{}, err
	}
	r.s.st.users[id] = u
	return u, nil
}

func (r *userRepo) UpdateLastLogin(_ context.Context, id uuid.UUID, at time.Time) error {
	_, err := r.update(id, func(u *auth.User) error { u.LastLoginAt = &at; u.UpdatedAt = at; return nil })
	return err
}

func (r *userRepo) SetPasswordHash(_ context.Context, id uuid.UUID, hash string, changedAt *time.Time) error {
	_, err := r.update(id, func(u *auth.User) error {
		u.PasswordHash = hash
		if changedAt != nil {
			u.PasswordChangedAt = *changedAt
			u.UpdatedAt = *changedAt
		}
		return nil
	})
	return err
}

func (r *userRepo) UpdateConsents(_ context.Context, id uuid.UUID, termsVersion string, at time.Time) (auth.User, error) {
	return r.update(id, func(u *auth.User) error {
		if u.Status != auth.UserStatusActive {
			return auth.ErrUserNotActive
		}
		if u.TermsVersion != termsVersion {
			u.TermsAcceptedAt = at
		}
		u.TermsVersion, u.UpdatedAt = termsVersion, at
		return nil
	})
}

func (r *userRepo) UpdateStatus(_ context.Context, id uuid.UUID, to auth.UserStatus, at time.Time) error {
	_, err := r.update(id, func(u *auth.User) error {
		u.Status, u.UpdatedAt = to, at
		if to == auth.UserStatusDeleted {
			u.DeletedAt = &at
		} else {
			u.DeletedAt = nil
		}
		return nil
	})
	return err
}

func (r *userRepo) List(_ context.Context, f auth.UserFilter, after *uuid.UUID, limit int) ([]auth.User, error) {
	var out []auth.User
	for _, u := range r.s.st.users {
		switch {
		case f.EmailPrefix != "" && !strings.HasPrefix(strings.ToLower(u.Email), strings.ToLower(f.EmailPrefix)):
		case f.Status != nil && u.Status != *f.Status:
		case f.Role != nil && u.Role != *f.Role:
		case after != nil && bytes.Compare(u.ID[:], after[:]) >= 0:
		default:
			out = append(out, u)
		}
	}
	slices.SortFunc(out, func(a, b auth.User) int { return bytes.Compare(b.ID[:], a.ID[:]) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// ---- refresh tokens ----

type refreshRepo struct{ s *AuthStore }

func (r *refreshRepo) Insert(_ context.Context, t auth.RefreshToken) error {
	if err := r.s.Take("RefreshTokens.Insert"); err != nil {
		return err
	}
	for _, o := range r.s.st.refresh {
		if bytes.Equal(o.TokenHash, t.TokenHash) {
			return errors.New("fakes: duplicate refresh token hash")
		}
	}
	r.s.st.refresh[t.ID] = t
	return nil
}

func (r *refreshRepo) GetByID(_ context.Context, id uuid.UUID) (auth.RefreshToken, error) {
	t, ok := r.s.st.refresh[id]
	if !ok {
		return auth.RefreshToken{}, auth.ErrRefreshTokenNotFound
	}
	return t, nil
}

func (r *refreshRepo) LockByHash(_ context.Context, hash []byte) (auth.RefreshToken, error) {
	if err := r.s.Take("RefreshTokens.LockByHash"); err != nil {
		return auth.RefreshToken{}, err
	}
	for _, t := range r.s.st.refresh {
		if bytes.Equal(t.TokenHash, hash) {
			return t, nil
		}
	}
	return auth.RefreshToken{}, auth.ErrRefreshTokenNotFound
}

func (r *refreshRepo) MarkRotated(_ context.Context, id, replacedBy uuid.UUID, at time.Time) error {
	t, ok := r.s.st.refresh[id]
	if !ok {
		return auth.ErrRefreshTokenNotFound
	}
	reason := "ROTATED"
	t.RevokedAt, t.RevokeReason, t.ReplacedByID = &at, &reason, &replacedBy
	r.s.st.refresh[id] = t
	return nil
}

func (r *refreshRepo) revoke(match func(auth.RefreshToken) bool, reason string, at time.Time) int64 {
	var n int64
	for id, t := range r.s.st.refresh {
		if t.RevokedAt == nil && match(t) {
			t.RevokedAt, t.RevokeReason = &at, &reason
			r.s.st.refresh[id] = t
			n++
		}
	}
	return n
}

func (r *refreshRepo) RevokeFamily(_ context.Context, familyID uuid.UUID, reason string, at time.Time) (int64, error) {
	return r.revoke(func(t auth.RefreshToken) bool { return t.FamilyID == familyID }, reason, at), nil
}

func (r *refreshRepo) RevokeAllForUser(_ context.Context, userID uuid.UUID, reason string, at time.Time) (int64, error) {
	return r.revoke(func(t auth.RefreshToken) bool { return t.UserID == userID }, reason, at), nil
}

func (r *refreshRepo) DeleteAllForUser(_ context.Context, userID uuid.UUID) error {
	for id, t := range r.s.st.refresh {
		if t.UserID == userID {
			delete(r.s.st.refresh, id)
		}
	}
	return nil
}

func (r *refreshRepo) CountActive(_ context.Context, userID uuid.UUID, now time.Time) (int, error) {
	n := 0
	for _, t := range r.s.st.refresh {
		if t.UserID == userID && t.RevokedAt == nil && t.ExpiresAt.After(now) {
			n++
		}
	}
	return n, nil
}

// ---- password reset tokens ----

type resetRepo struct{ s *AuthStore }

func open(t auth.PasswordResetToken) bool { return t.UsedAt == nil && t.InvalidatedAt == nil }

func (r *resetRepo) InvalidateOpenForUser(_ context.Context, userID uuid.UUID, at time.Time) error {
	for id, t := range r.s.st.resets {
		if t.UserID == userID && open(t) {
			t.InvalidatedAt = &at
			r.s.st.resets[id] = t
		}
	}
	return nil
}

func (r *resetRepo) Insert(_ context.Context, t auth.PasswordResetToken) error {
	if err := r.s.Take("PasswordResets.Insert"); err != nil {
		return err
	}
	r.s.st.resets[t.ID] = t
	return nil
}

func (r *resetRepo) lookup(hash []byte) (auth.ResetLookup, error) {
	for _, t := range r.s.st.resets {
		if bytes.Equal(t.TokenHash, hash) {
			u := r.s.st.users[t.UserID]
			return auth.ResetLookup{Token: t, Email: u.Email, UserStatus: u.Status}, nil
		}
	}
	return auth.ResetLookup{}, auth.ErrResetTokenNotFound
}

func (r *resetRepo) FindByHash(_ context.Context, hash []byte) (auth.ResetLookup, error) {
	return r.lookup(hash)
}

func (r *resetRepo) LockByHash(_ context.Context, hash []byte) (auth.ResetLookup, error) {
	if err := r.s.Take("PasswordResets.LockByHash"); err != nil {
		return auth.ResetLookup{}, err
	}
	return r.lookup(hash)
}

func (r *resetRepo) MarkUsed(_ context.Context, id uuid.UUID, at time.Time) error {
	t, ok := r.s.st.resets[id]
	if !ok {
		return auth.ErrResetTokenNotFound
	}
	t.UsedAt = &at
	r.s.st.resets[id] = t
	return nil
}

func (r *resetRepo) InvalidateOthers(_ context.Context, userID, exceptID uuid.UUID, at time.Time) error {
	for id, t := range r.s.st.resets {
		if t.UserID == userID && id != exceptID && open(t) {
			t.InvalidatedAt = &at
			r.s.st.resets[id] = t
		}
	}
	return nil
}

// MutateUser edits a stored user outside any transaction (test setup, e.g. promote to ADMIN).
func (s *AuthStore) MutateUser(id uuid.UUID, fn func(*auth.User)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u := s.st.users[id]
	fn(&u)
	s.st.users[id] = u
}

// ResetTokens returns every stored password reset token.
func (s *AuthStore) ResetTokens() []auth.PasswordResetToken {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Collect(maps.Values(s.st.resets))
}
