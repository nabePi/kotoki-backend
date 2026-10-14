package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net/netip"
	"time"

	"github.com/google/uuid"

	"github.com/nabePi/kotoki-backend/internal/platform/clock"
	"github.com/nabePi/kotoki-backend/internal/platform/errs"
	"github.com/nabePi/kotoki-backend/internal/platform/idgen"
)

// Config holds the auth settings (spec 3.14).
type Config struct {
	TermsVersion          string
	InitialBalanceSeconds int64
	AccessTTL             time.Duration
	RefreshTTL            time.Duration
	RefreshReuseGrace     time.Duration
	ResetTTL              time.Duration
	ResetURLBase          string
	Rand                  io.Reader // defaults to crypto/rand.Reader
}

// Events records auth outcomes for metrics; a nil Events is a no-op.
type Events interface{ Record(event, result string) }

// Service implements register, login, refresh, logout and password reset.
type Service struct {
	store      Store
	hasher     PasswordHasher
	tokens     TokenIssuer
	mail       MailQueue
	clk        clock.Clock
	ids        idgen.Generator
	log        *slog.Logger
	events     Events
	cfg        Config
	dummyHash  string
	passPolicy *PasswordPolicy
}

type noopEvents struct{}

func (noopEvents) Record(string, string) {}

// NewService builds the service and the dummy hash used to equalize login timing.
func NewService(store Store, hasher PasswordHasher, tokens TokenIssuer, mail MailQueue, clk clock.Clock,
	ids idgen.Generator, log *slog.Logger, events Events, cfg Config) (*Service, error) {
	if cfg.Rand == nil {
		cfg.Rand = rand.Reader
	}
	if events == nil {
		events = noopEvents{}
	}
	dummy, err := hasher.Hash(context.Background(), "dummy-password-for-timing-equalization")
	if err != nil {
		return nil, err
	}
	return &Service{store: store, hasher: hasher, tokens: tokens, mail: mail, clk: clk, ids: ids, log: log,
		events: events, cfg: cfg, dummyHash: dummy, passPolicy: defaultPolicy}, nil
}

// TermsVersion is the current terms/privacy version (PRIVACY_POLICY_VERSION).
func (s *Service) TermsVersion() string { return s.cfg.TermsVersion }

// RegisterInput is the registration request.
type RegisterInput struct {
	Email, Password, FullName string
	PhoneNumber               *string
	TermsVersion              string
	UserAgent                 string
	IP                        netip.Addr
}

// LoginInput is the login request.
type LoginInput struct {
	Email, Password, UserAgent string
	IP                         netip.Addr
}

// AuthResult is returned by Register and Login.
type AuthResult struct { //nolint:revive // name fixed by the plan; httpapi refers to auth.AuthResult
	AccessToken      string
	ExpiresIn        time.Duration
	RefreshToken     string
	RefreshExpiresAt time.Time
	User             User
}

// newOpaqueToken returns a 32-byte random token (43 base64url chars) and its SHA-256.
func newOpaqueToken(r io.Reader) (raw string, hash []byte, err error) {
	b := make([]byte, 32)
	if _, err := io.ReadFull(r, b); err != nil {
		return "", nil, errs.Wrap(err, errs.CodeInternalError, "token generation failed")
	}
	sum := sha256.Sum256(b)
	return base64.RawURLEncoding.EncodeToString(b), sum[:], nil
}

func uaHash(ua string) string {
	sum := sha256.Sum256([]byte(ua))
	return hex.EncodeToString(sum[:])[:16]
}

func (s *Service) audit(ctx context.Context, event, result string, userID uuid.UUID, ip netip.Addr, ua string, extra ...any) {
	args := []any{"audit", true, "audit_event", event, "result", result, "ip", ip.String(), "user_agent_hash", uaHash(ua)}
	if userID != uuid.Nil {
		args = append(args, "user_id", userID.String())
	}
	s.log.InfoContext(ctx, "audit", append(args, extra...)...)
}

func termsError(reason string) error {
	return errs.WithDetails(errs.New(errs.CodeValidationFailed, "invalid terms_version"),
		map[string]any{"fields": []map[string]any{{"field": "terms_version", "reason": reason}}})
}

func ipPtr(ip netip.Addr) *netip.Addr {
	if !ip.IsValid() {
		return nil
	}
	return &ip
}

func uaPtr(ua string) *string {
	if ua == "" {
		return nil
	}
	if len(ua) > 512 {
		ua = ua[:512]
	}
	return &ua
}

// issueFamily inserts the first token of a new refresh family inside tx.
func (s *Service) issueFamily(ctx context.Context, tx Tx, userID uuid.UUID, ua string, ip netip.Addr, now time.Time) (raw string, tok RefreshToken, err error) {
	raw, hash, err := newOpaqueToken(s.cfg.Rand)
	if err != nil {
		return "", RefreshToken{}, err
	}
	id := s.ids.New()
	tok = RefreshToken{ID: id, UserID: userID, FamilyID: id, TokenHash: hash, UserAgent: uaPtr(ua), IP: ipPtr(ip),
		CreatedAt: now, ExpiresAt: now.Add(s.cfg.RefreshTTL)}
	return raw, tok, tx.RefreshTokens().Insert(ctx, tok)
}

func (s *Service) accessFor(u User, family uuid.UUID, now time.Time) (string, time.Duration, error) {
	tok, exp, err := s.tokens.IssueAccess(AccessSubject{UserID: u.ID, Role: u.Role, FamilyID: family}, now)
	if err != nil {
		return "", 0, errs.Wrap(err, errs.CodeInternalError, "access token issue failed")
	}
	return tok, exp.Sub(now), nil
}

// Register creates a user, its balance account and the first refresh family (spec 3.8.1).
func (s *Service) Register(ctx context.Context, in RegisterInput) (res AuthResult, err error) {
	defer func() {
		result := "success"
		if err != nil {
			result = "failure"
		}
		s.events.Record("register", result)
		s.audit(ctx, "register", result, res.User.ID, in.IP, in.UserAgent)
	}()

	email, err := NormalizeEmail(in.Email)
	if err != nil {
		return AuthResult{}, err
	}
	name, err := NormalizeFullName(in.FullName)
	if err != nil {
		return AuthResult{}, err
	}
	var phone *string
	if in.PhoneNumber != nil && *in.PhoneNumber != "" {
		p, perr := NormalizePhone(*in.PhoneNumber)
		if perr != nil {
			return AuthResult{}, perr
		}
		phone = &p
	}
	if err := s.passPolicy.Validate(in.Password, email); err != nil {
		return AuthResult{}, err
	}
	switch {
	case in.TermsVersion == "":
		return AuthResult{}, termsError("required")
	case in.TermsVersion != s.cfg.TermsVersion:
		return AuthResult{}, termsError("not_current")
	}
	hash, err := s.hasher.Hash(ctx, in.Password)
	if err != nil {
		return AuthResult{}, err
	}

	now := s.clk.Now()
	user := User{
		ID: s.ids.New(), Email: email, PhoneNumber: phone, FullName: name, PasswordHash: hash,
		Role: RoleUser, Status: UserStatusActive, PasswordChangedAt: now,
		TermsVersion: in.TermsVersion, TermsAcceptedAt: now, CreatedAt: now, UpdatedAt: now,
	}
	var raw string
	var tok RefreshToken
	err = s.store.WithinTx(ctx, func(ctx context.Context, tx Tx) error {
		if err := tx.Users().Insert(ctx, user); err != nil {
			return err
		}
		if err := tx.Accounts().OpenAccount(ctx, user.ID, s.cfg.InitialBalanceSeconds, now); err != nil {
			return err
		}
		var ierr error
		raw, tok, ierr = s.issueFamily(ctx, tx, user.ID, in.UserAgent, in.IP, now)
		return ierr
	})
	switch {
	case errors.Is(err, ErrEmailTaken):
		return AuthResult{}, errs.New(errs.CodeEmailAlreadyRegistered, "email already registered")
	case errors.Is(err, ErrPhoneTaken):
		return AuthResult{}, errs.New(errs.CodePhoneAlreadyRegistered, "phone number already registered")
	case err != nil:
		return AuthResult{}, err
	}
	access, expiresIn, err := s.accessFor(user, tok.FamilyID, now)
	if err != nil {
		return AuthResult{}, err
	}
	return AuthResult{AccessToken: access, ExpiresIn: expiresIn, RefreshToken: raw, RefreshExpiresAt: tok.ExpiresAt, User: user}, nil
}

// Login authenticates by email and password (spec 3.8.2).
func (s *Service) Login(ctx context.Context, in LoginInput) (res AuthResult, err error) {
	defer func() {
		result, event := "success", "login_succeeded"
		if err != nil {
			result, event = "failure", "login_failed"
		}
		s.events.Record("login", result)
		s.audit(ctx, event, result, res.User.ID, in.IP, in.UserAgent)
	}()

	email, err := NormalizeEmail(in.Email)
	if err != nil {
		return AuthResult{}, err
	}
	var user User
	var found bool
	err = s.store.WithinTx(ctx, func(ctx context.Context, tx Tx) error {
		u, gerr := tx.Users().GetByEmail(ctx, email)
		switch {
		case errors.Is(gerr, ErrUserNotFound):
			return nil
		case gerr != nil:
			return gerr
		}
		user, found = u, true
		return nil
	})
	if err != nil {
		return AuthResult{}, err
	}
	if !found || user.Status == UserStatusDeleted {
		if _, verr := s.hasher.Verify(ctx, in.Password, s.dummyHash); verr != nil {
			return AuthResult{}, verr
		}
		return AuthResult{}, errs.New(errs.CodeInvalidCredentials, "invalid credentials")
	}
	ok, err := s.hasher.Verify(ctx, in.Password, user.PasswordHash)
	if err != nil {
		return AuthResult{}, err
	}
	if !ok {
		return AuthResult{}, errs.New(errs.CodeInvalidCredentials, "invalid credentials")
	}
	if user.Status == UserStatusSuspended {
		return AuthResult{}, errs.New(errs.CodeAccountSuspended, "account suspended")
	}

	now := s.clk.Now()
	var raw string
	var tok RefreshToken
	err = s.store.WithinTx(ctx, func(ctx context.Context, tx Tx) error {
		if err := tx.Users().UpdateLastLogin(ctx, user.ID, now); err != nil {
			return err
		}
		var ierr error
		raw, tok, ierr = s.issueFamily(ctx, tx, user.ID, in.UserAgent, in.IP, now)
		return ierr
	})
	if err != nil {
		return AuthResult{}, err
	}
	if s.hasher.NeedsRehash(user.PasswordHash) {
		s.rehash(ctx, user.ID, in.Password)
	}
	user.LastLoginAt = &now
	access, expiresIn, err := s.accessFor(user, tok.FamilyID, now)
	if err != nil {
		return AuthResult{}, err
	}
	return AuthResult{AccessToken: access, ExpiresIn: expiresIn, RefreshToken: raw, RefreshExpiresAt: tok.ExpiresAt, User: user}, nil
}

// rehash upgrades the stored hash after a successful login; failures are only logged.
func (s *Service) rehash(ctx context.Context, userID uuid.UUID, password string) {
	hash, err := s.hasher.Hash(ctx, password)
	if err == nil {
		err = s.store.WithinTx(ctx, func(ctx context.Context, tx Tx) error {
			return tx.Users().SetPasswordHash(ctx, userID, hash, nil)
		})
	}
	if err != nil {
		s.log.WarnContext(ctx, "password rehash failed", "user_id", userID.String(), "error_code", string(errs.CodeOf(err)))
	}
}

// decodeToken validates the shape of a client token and returns its SHA-256.
func decodeToken(raw string) ([]byte, bool) {
	if len(raw) != 43 {
		return nil, false
	}
	b, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil || len(b) != 32 {
		return nil, false
	}
	sum := sha256.Sum256(b)
	return sum[:], true
}

// RefreshInput is the refresh request (cookie value plus client metadata).
type RefreshInput struct {
	RefreshToken, UserAgent string
	IP                      netip.Addr
}

// RefreshResult is returned by Refresh.
type RefreshResult struct {
	AccessToken      string
	ExpiresIn        time.Duration
	RefreshToken     string
	RefreshExpiresAt time.Time
}

func invalidRefresh() error { return errs.New(errs.CodeRefreshTokenInvalid, "refresh token invalid") }

// Refresh rotates a refresh token (spec 3.8.3). Reuse outside the grace window
// and status changes revoke the family and still commit before the 401 is returned.
func (s *Service) Refresh(ctx context.Context, in RefreshInput) (res RefreshResult, err error) {
	event := "refresh"
	defer func() {
		result := "success"
		if err != nil {
			result = "failure"
		}
		s.events.Record(event, result)
	}()

	hash, ok := decodeToken(in.RefreshToken)
	if !ok {
		return RefreshResult{}, invalidRefresh()
	}
	now := s.clk.Now()
	var (
		outErr error // returned after the transaction committed
		raw    string
		next   RefreshToken
		user   User
	)
	err = s.store.WithinTx(ctx, func(ctx context.Context, tx Tx) error {
		old, err := tx.RefreshTokens().LockByHash(ctx, hash)
		if errors.Is(err, ErrRefreshTokenNotFound) {
			return invalidRefresh()
		}
		if err != nil {
			return err
		}
		if old.RevokedAt != nil {
			if old.RevokeReason != nil && *old.RevokeReason == "ROTATED" && old.ReplacedByID != nil &&
				!old.RevokedAt.Before(now.Add(-s.cfg.RefreshReuseGrace)) {
				repl, gerr := tx.RefreshTokens().GetByID(ctx, *old.ReplacedByID)
				if gerr == nil && repl.RevokedAt == nil {
					return errs.New(errs.CodeRefreshTokenRotated, "refresh token was just rotated")
				}
			}
			if _, rerr := tx.RefreshTokens().RevokeFamily(ctx, old.FamilyID, "REUSE_DETECTED", now); rerr != nil {
				return rerr
			}
			event = "refresh_reuse"
			s.log.WarnContext(ctx, "refresh_reuse_detected", "audit", true, "audit_event", "refresh_reuse_detected",
				"user_id", old.UserID.String(), "family_id", old.FamilyID.String(), "ip", in.IP.String(), "user_agent_hash", uaHash(in.UserAgent))
			outErr = invalidRefresh()
			return nil
		}
		if !old.ExpiresAt.After(now) {
			return invalidRefresh()
		}
		u, err := tx.Users().GetByID(ctx, old.UserID)
		if err != nil {
			return err
		}
		if u.Status != UserStatusActive {
			if _, rerr := tx.RefreshTokens().RevokeFamily(ctx, old.FamilyID, "USER_STATUS_CHANGED", now); rerr != nil {
				return rerr
			}
			outErr = invalidRefresh()
			return nil
		}
		var rerr error
		raw, rerr = s.rotate(ctx, tx, old, in, now, &next)
		user = u
		return rerr
	})
	if err != nil {
		return RefreshResult{}, err
	}
	if outErr != nil {
		return RefreshResult{}, outErr
	}
	access, expiresIn, err := s.accessFor(user, next.FamilyID, now)
	if err != nil {
		return RefreshResult{}, err
	}
	return RefreshResult{AccessToken: access, ExpiresIn: expiresIn, RefreshToken: raw, RefreshExpiresAt: next.ExpiresAt}, nil
}

func (s *Service) rotate(ctx context.Context, tx Tx, old RefreshToken, in RefreshInput, now time.Time, out *RefreshToken) (string, error) {
	raw, hash, err := newOpaqueToken(s.cfg.Rand)
	if err != nil {
		return "", err
	}
	parent := old.ID
	*out = RefreshToken{ID: s.ids.New(), UserID: old.UserID, FamilyID: old.FamilyID, ParentID: &parent, TokenHash: hash,
		UserAgent: uaPtr(in.UserAgent), IP: ipPtr(in.IP), CreatedAt: now, ExpiresAt: now.Add(s.cfg.RefreshTTL)}
	if err := tx.RefreshTokens().Insert(ctx, *out); err != nil {
		return "", err
	}
	if err := tx.RefreshTokens().MarkRotated(ctx, old.ID, out.ID, now); err != nil {
		return "", err
	}
	return raw, nil
}

// Logout revokes the family of refreshToken. It is idempotent: an unknown or
// malformed token is not an error (spec 3.8.4).
func (s *Service) Logout(ctx context.Context, refreshToken string) error {
	hash, ok := decodeToken(refreshToken)
	if !ok {
		return nil
	}
	now := s.clk.Now()
	var revoked *RefreshToken
	err := s.store.WithinTx(ctx, func(ctx context.Context, tx Tx) error {
		tk, err := tx.RefreshTokens().LockByHash(ctx, hash)
		if errors.Is(err, ErrRefreshTokenNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if _, err := tx.RefreshTokens().RevokeFamily(ctx, tk.FamilyID, "LOGOUT", now); err != nil {
			return err
		}
		revoked = &tk
		return nil
	})
	if err != nil {
		return err
	}
	if revoked != nil {
		s.audit(ctx, "logout", "success", revoked.UserID, netip.Addr{}, "", "family_id", revoked.FamilyID.String())
	}
	return nil
}
