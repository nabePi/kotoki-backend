package auth

import (
	"context"
	"errors"
	"log/slog"

	"github.com/google/uuid"

	"github.com/nabePi/kotoki-backend/internal/platform/clock"
	"github.com/nabePi/kotoki-backend/internal/platform/errs"
)

// UserService serves the authenticated user's own profile, consents and account deletion.
type UserService struct {
	store        Store
	hasher       PasswordHasher
	deleter      AccountDeleter
	clk          clock.Clock
	log          *slog.Logger
	termsVersion string
}

// NewUserService returns a UserService; deleter performs steps 2–4 of account deletion.
func NewUserService(store Store, hasher PasswordHasher, deleter AccountDeleter, clk clock.Clock, log *slog.Logger, termsVersion string) *UserService {
	return &UserService{store: store, hasher: hasher, deleter: deleter, clk: clk, log: log, termsVersion: termsVersion}
}

// CurrentTermsVersion is the current terms/privacy version.
func (s *UserService) CurrentTermsVersion() string { return s.termsVersion }

func (s *UserService) load(ctx context.Context, userID uuid.UUID) (User, error) {
	var u User
	err := s.store.WithinTx(ctx, func(ctx context.Context, tx Tx) error {
		var err error
		u, err = tx.Users().GetByID(ctx, userID)
		return err
	})
	if errors.Is(err, ErrUserNotFound) {
		return User{}, errs.New(errs.CodeUserNotFound, "user not found")
	}
	return u, err
}

// Me returns the user record.
func (s *UserService) Me(ctx context.Context, userID uuid.UUID) (User, error) {
	return s.load(ctx, userID)
}

// RequireActive loads the user and fails with ACCOUNT_SUSPENDED unless it is ACTIVE
// (SUSPENDED and DELETED alike, spec 3.7).
func (s *UserService) RequireActive(ctx context.Context, userID uuid.UUID) (User, error) {
	u, err := s.load(ctx, userID)
	if err != nil {
		return User{}, err
	}
	if u.Status != UserStatusActive {
		return User{}, errs.New(errs.CodeAccountSuspended, "account suspended")
	}
	return u, nil
}

// ConsentsInput is a PUT /me/consents request.
type ConsentsInput struct {
	UserID       uuid.UUID
	TermsVersion string
}

// UpdateConsents records acceptance of the current terms version (spec 3.13.2).
func (s *UserService) UpdateConsents(ctx context.Context, in ConsentsInput) (User, error) {
	if in.TermsVersion != s.termsVersion {
		return User{}, termsError("not_current")
	}
	now := s.clk.Now()
	var u User
	err := s.store.WithinTx(ctx, func(ctx context.Context, tx Tx) error {
		var err error
		u, err = tx.Users().UpdateConsents(ctx, in.UserID, in.TermsVersion, now)
		return err
	})
	switch {
	case errors.Is(err, ErrUserNotActive):
		return User{}, errs.New(errs.CodeAccountSuspended, "account suspended")
	case errors.Is(err, ErrUserNotFound):
		return User{}, errs.New(errs.CodeUserNotFound, "user not found")
	case err != nil:
		return User{}, err
	}
	s.log.InfoContext(ctx, "audit", "audit", true, "audit_event", "consent_updated", "result", "success",
		"user_id", in.UserID.String(), "terms_version", in.TermsVersion)
	return u, nil
}

// DeleteAccount verifies the password and hands over to the AccountDeleter (spec 3.13.3).
func (s *UserService) DeleteAccount(ctx context.Context, userID uuid.UUID, password string) error {
	u, err := s.RequireActive(ctx, userID)
	if err != nil {
		return err
	}
	ok, err := s.hasher.Verify(ctx, password, u.PasswordHash)
	if err != nil {
		return err
	}
	if !ok {
		return errs.New(errs.CodeInvalidCredentials, "invalid credentials")
	}
	if err := s.deleter.DeleteAccount(ctx, userID, s.clk.Now()); err != nil {
		return err
	}
	s.log.InfoContext(ctx, "audit", "audit", true, "audit_event", "account_deletion_requested", "result", "success",
		"user_id", userID.String())
	return nil
}
