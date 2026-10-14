package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/nabePi/kotoki-backend/internal/auth"
	"github.com/nabePi/kotoki-backend/internal/platform/errs"
	"github.com/nabePi/kotoki-backend/internal/platform/ratelimit"
)

const refreshCookieName = "kotoki_refresh"

type authHandler struct {
	svc     authService
	users   userService
	limiter ratelimit.Limiter
	log     *slog.Logger
	cfg     Config
}

// newRefreshCookie builds the refresh cookie (spec 3.8.5); an empty value clears it.
// Secure is configuration-driven: AUTH_REFRESH_COOKIE_SECURE=false is only accepted in local.
func newRefreshCookie(cfg Config, value string, maxAge time.Duration) *http.Cookie {
	secs := int(maxAge / time.Second)
	if value == "" {
		secs = -1 // renders as Max-Age=0
	}
	return &http.Cookie{ //nolint:gosec // G124: Secure comes from config and is validated (true outside local)
		Name: refreshCookieName, Value: value, Path: "/api/v1/auth", Domain: cfg.Cookie.Domain,
		MaxAge: secs, HttpOnly: true, Secure: cfg.Cookie.Secure, SameSite: http.SameSiteStrictMode,
	}
}

func (h *authHandler) setRefresh(req interface{ SetCookie(*http.Cookie) }, token string) {
	req.SetCookie(newRefreshCookie(h.cfg, token, h.cfg.Cookie.MaxAge))
}

func (h *authHandler) clearRefresh(req interface{ SetCookie(*http.Cookie) }) {
	req.SetCookie(newRefreshCookie(h.cfg, "", 0))
}

// allowEmail applies a per-email policy; the subject is a hash, never the address.
func (h *authHandler) allowEmail(ctx context.Context, policy, email string) (bool, time.Duration) {
	d, err := h.limiter.Allow(ctx, ratelimit.Get(policy), ratelimit.SubjectEmail(email))
	if err != nil {
		h.log.WarnContext(ctx, "rate limiter error, allowing request", "policy", policy, "error", err.Error())
		return true, 0
	}
	return d.Allowed, d.RetryAfter
}

func (h *authHandler) register(ctx context.Context, req Request[registerRequest]) (authResponse, error) {
	res, err := h.svc.Register(ctx, auth.RegisterInput{
		Email: req.Body.Email, Password: req.Body.Password, FullName: req.Body.FullName, PhoneNumber: req.Body.PhoneNumber,
		TermsVersion: req.Body.TermsVersion, UserAgent: req.UserAgent, IP: req.ClientIP,
	})
	if err != nil {
		return authResponse{}, err
	}
	h.setRefresh(req, res.RefreshToken)
	return newAuthResponse(res, h.svc.TermsVersion()), nil
}

func (h *authHandler) login(ctx context.Context, req Request[loginRequest]) (authResponse, error) {
	if ok, retry := h.allowEmail(ctx, "auth_login_email", req.Body.Email); !ok {
		secs := int((retry + time.Second - 1) / time.Second)
		req.SetHeader("Retry-After", strconv.Itoa(max(secs, 1)))
		return authResponse{}, errs.New(errs.CodeRateLimited, "rate limited: auth_login_email")
	}
	res, err := h.svc.Login(ctx, auth.LoginInput{Email: req.Body.Email, Password: req.Body.Password, UserAgent: req.UserAgent, IP: req.ClientIP})
	if err != nil {
		return authResponse{}, err
	}
	h.setRefresh(req, res.RefreshToken)
	return newAuthResponse(res, h.svc.TermsVersion()), nil
}

func (h *authHandler) refresh(ctx context.Context, req Request[struct{}]) (refreshResponse, error) {
	res, err := h.svc.Refresh(ctx, auth.RefreshInput{RefreshToken: req.Cookie(refreshCookieName), UserAgent: req.UserAgent, IP: req.ClientIP})
	if err != nil {
		if errs.CodeOf(err) == errs.CodeRefreshTokenInvalid {
			h.clearRefresh(req) // a rotated (409) token keeps its cookie: the newest one is already in the browser
		}
		return refreshResponse{}, err
	}
	h.setRefresh(req, res.RefreshToken)
	return refreshResponse{AccessToken: res.AccessToken, TokenType: "Bearer", ExpiresIn: int(res.ExpiresIn / time.Second)}, nil
}

func (h *authHandler) logout(ctx context.Context, req Request[struct{}]) (NoContent, error) {
	h.clearRefresh(req)
	return NoContent{}, h.svc.Logout(ctx, req.Cookie(refreshCookieName))
}

func (h *authHandler) forgot(ctx context.Context, req Request[forgotRequest]) (statusResponse, error) {
	allowed, _ := h.allowEmail(ctx, "auth_forgot_email", req.Body.Email)
	err := h.svc.ForgotPassword(ctx, auth.ForgotInput{Email: req.Body.Email, IP: req.ClientIP, EmailLimited: !allowed})
	if err != nil {
		return statusResponse{}, err
	}
	return statusResponse{Status: "ACCEPTED"}, nil
}

func (h *authHandler) reset(ctx context.Context, req Request[resetRequest]) (NoContent, error) {
	return NoContent{}, h.svc.ResetPassword(ctx, auth.ResetInput{Token: req.Body.Token, NewPassword: req.Body.NewPassword})
}

func (h *authHandler) terms(_ context.Context, _ Request[struct{}]) (termsResponse, error) {
	return termsResponse{TermsVersion: h.svc.TermsVersion()}, nil
}
