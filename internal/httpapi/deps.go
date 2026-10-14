package httpapi

import (
	"context"
	"log/slog"
	"net/netip"
	"time"

	"github.com/google/uuid"

	"github.com/nabePi/kotoki-backend/internal/auth"
	"github.com/nabePi/kotoki-backend/internal/platform/clock"
	"github.com/nabePi/kotoki-backend/internal/platform/metrics"
	"github.com/nabePi/kotoki-backend/internal/platform/ratelimit"
)

// CookieConfig describes the refresh-token cookie.
type CookieConfig struct {
	Secure bool
	Domain string
	MaxAge time.Duration
}

// Config holds the HTTP-layer settings.
type Config struct {
	CORSOrigins    []string
	TrustedProxies []netip.Prefix
	BodyLimit      int64
	Cookie         CookieConfig
}

// authService is what the auth handlers need; *auth.Service satisfies it.
type authService interface {
	Register(context.Context, auth.RegisterInput) (auth.AuthResult, error)
	Login(context.Context, auth.LoginInput) (auth.AuthResult, error)
	Refresh(context.Context, auth.RefreshInput) (auth.RefreshResult, error)
	Logout(context.Context, string) error
	ForgotPassword(context.Context, auth.ForgotInput) error
	ResetPassword(context.Context, auth.ResetInput) error
	TermsVersion() string
}

// userService is what the /me handlers need; *auth.UserService satisfies it.
type userService interface {
	Me(context.Context, uuid.UUID) (auth.User, error)
	UpdateConsents(context.Context, auth.ConsentsInput) (auth.User, error)
	DeleteAccount(ctx context.Context, userID uuid.UUID, password string) error
	CurrentTermsVersion() string
}

// Deps are the collaborators of NewRouter, wired by hand in cmd/api.
type Deps struct {
	Log       *slog.Logger
	Cfg       Config
	Limiter   ratelimit.Limiter
	Tokens    auth.TokenIssuer
	Clock     clock.Clock
	Metrics   *metrics.HTTP // optional
	Auth      authService
	Users     userService
	AdminGate AdminGate
}
