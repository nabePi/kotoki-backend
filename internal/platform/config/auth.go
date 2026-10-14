package config

import (
	"encoding/base64"
	"fmt"
	"net/netip"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// Argon2 holds the password hashing cost (spec 3.4).
type Argon2 struct {
	MemoryKiB     uint32 `env:"AUTH_ARGON2_MEMORY_KIB"`
	Iterations    uint32 `env:"AUTH_ARGON2_ITERATIONS"`
	Parallelism   uint8  `env:"AUTH_ARGON2_PARALLELISM"`
	MaxConcurrent int    `env:"AUTH_ARGON2_MAX_CONCURRENT"`
}

// Mail selects and configures the mail adapter (spec 3.9.3).
type Mail struct {
	Mode         string `env:"MAIL_MODE"`
	From         string `env:"MAIL_FROM"`
	SMTPHost     string `env:"SMTP_HOST"`
	SMTPPort     int    `env:"SMTP_PORT"`
	SMTPUsername string `env:"SMTP_USERNAME"`
	SMTPPassword Secret `env:"SMTP_PASSWORD"`
}

// Auth holds identity, token, ticket and mail settings of the api binary (spec 13.2.7).
type Auth struct {
	AuthJWTSecret         Secret        `env:"AUTH_JWT_SECRET"`
	AuthJWTPreviousSecret Secret        `env:"AUTH_JWT_PREVIOUS_SECRET"`
	JWTIssuer             string        `env:"AUTH_JWT_ISSUER"`
	JWTAudience           string        `env:"AUTH_JWT_AUDIENCE"`
	AccessTokenTTL        time.Duration `env:"AUTH_ACCESS_TOKEN_TTL"`
	RefreshTokenTTL       time.Duration `env:"AUTH_REFRESH_TOKEN_TTL"`
	RefreshReuseGrace     time.Duration `env:"AUTH_REFRESH_REUSE_GRACE"`
	RefreshCookieSecure   bool          `env:"AUTH_REFRESH_COOKIE_SECURE"`
	RefreshCookieDomain   string        `env:"AUTH_REFRESH_COOKIE_DOMAIN"`
	PasswordResetTTL      time.Duration `env:"AUTH_PASSWORD_RESET_TTL"`
	Argon2                Argon2
	WSTicketTTL           time.Duration `env:"WS_TICKET_TTL"`
	RealtimePublicURL     string        `env:"REALTIME_PUBLIC_URL"`
	InitialBalanceSeconds int64         `env:"INITIAL_BALANCE_SECONDS"`
	PrivacyPolicyVersion  string        `env:"PRIVACY_POLICY_VERSION"`
	PasswordResetURLBase  string        `env:"PASSWORD_RESET_URL_BASE"`
	Mail                  Mail
	TrustedProxyCIDRs     []string `env:"TRUSTED_PROXY_CIDRS" envSeparator:","`
}

func defaultAuth() Auth {
	return Auth{
		JWTIssuer: "kotoki-api", JWTAudience: "kotoki",
		AccessTokenTTL: 15 * time.Minute, RefreshTokenTTL: 720 * time.Hour, RefreshReuseGrace: 10 * time.Second,
		RefreshCookieSecure: true, PasswordResetTTL: 15 * time.Minute,
		Argon2:      Argon2{MemoryKiB: 19456, Iterations: 2, Parallelism: 1, MaxConcurrent: 8},
		WSTicketTTL: 30 * time.Second, RealtimePublicURL: "ws://localhost:8081",
		PasswordResetURLBase: defaultResetURLBase,
		Mail:                 Mail{Mode: "log", From: "Kotoki <no-reply@localhost>", SMTPPort: 587},
	}
}

func decodeSecret(s Secret) ([]byte, error) {
	raw := strings.TrimSpace(s.Reveal())
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if b, err := enc.DecodeString(raw); err == nil {
			return b, nil
		}
	}
	return nil, fmt.Errorf("not valid base64")
}

// JWTSecretBytes returns the decoded signing secret.
func (a Auth) JWTSecretBytes() ([]byte, error) { return decodeSecret(a.AuthJWTSecret) }

// JWTPreviousSecretBytes returns the decoded verification-only secret, or nil when unset.
func (a Auth) JWTPreviousSecretBytes() ([]byte, error) {
	if a.AuthJWTPreviousSecret == "" {
		return nil, nil
	}
	return decodeSecret(a.AuthJWTPreviousSecret)
}

// TrustedProxyPrefixes parses TRUSTED_PROXY_CIDRS (already validated by Validate).
func (a Auth) TrustedProxyPrefixes() []netip.Prefix {
	var out []netip.Prefix
	for _, c := range a.TrustedProxyCIDRs {
		if p, err := netip.ParsePrefix(strings.TrimSpace(c)); err == nil {
			out = append(out, p)
		}
	}
	return out
}

const defaultResetURLBase = "http://localhost:3000/reset-password"

var termsVersionRE = regexp.MustCompile(`^[A-Za-z0-9._-]{1,32}$`)

func (a Auth) validate(v *validator, c Common) {
	if a.AuthJWTSecret == "" {
		v.fail("AUTH_JWT_SECRET", "required")
	} else if b, err := a.JWTSecretBytes(); err != nil {
		v.fail("AUTH_JWT_SECRET", "must be base64")
	} else if len(b) < 32 {
		v.fail("AUTH_JWT_SECRET", "must decode to at least 32 bytes, got %d", len(b))
	}
	if a.AuthJWTPreviousSecret != "" {
		if b, err := a.JWTPreviousSecretBytes(); err != nil {
			v.fail("AUTH_JWT_PREVIOUS_SECRET", "must be base64")
		} else if len(b) < 32 {
			v.fail("AUTH_JWT_PREVIOUS_SECRET", "must decode to at least 32 bytes, got %d", len(b))
		}
	}
	switch {
	case a.PrivacyPolicyVersion == "":
		v.fail("PRIVACY_POLICY_VERSION", "required")
	case !termsVersionRE.MatchString(a.PrivacyPolicyVersion):
		v.fail("PRIVACY_POLICY_VERSION", "must match %s", termsVersionRE)
	}
	if !a.RefreshCookieSecure && c.AppEnv != "local" {
		v.fail("AUTH_REFRESH_COOKIE_SECURE", "false is only accepted when APP_ENV is local")
	}
	switch a.Mail.Mode {
	case "log":
		if c.strict() {
			v.fail("MAIL_MODE", "log is rejected when APP_ENV is staging or prod")
		}
	case "smtp":
		if a.Mail.SMTPHost == "" {
			v.fail("SMTP_HOST", "required when MAIL_MODE=smtp")
		}
	default:
		v.fail("MAIL_MODE", "must be log or smtp, got %q", a.Mail.Mode)
	}
	if c.strict() {
		u, err := url.Parse(a.PasswordResetURLBase)
		switch {
		case a.PasswordResetURLBase == defaultResetURLBase:
			v.fail("PASSWORD_RESET_URL_BASE", "required when APP_ENV is staging or prod (the localhost default is rejected)")
		case err != nil || u.Scheme != "https" || u.Host == "":
			v.fail("PASSWORD_RESET_URL_BASE", "must be an absolute https URL when APP_ENV is staging or prod")
		}
	}
	for _, cidr := range a.TrustedProxyCIDRs {
		if _, err := netip.ParsePrefix(strings.TrimSpace(cidr)); err != nil {
			v.fail("TRUSTED_PROXY_CIDRS", "invalid CIDR %q", cidr)
		}
	}
	if a.Argon2.MemoryKiB == 0 {
		v.fail("AUTH_ARGON2_MEMORY_KIB", "must be > 0")
	}
	if a.Argon2.Iterations == 0 {
		v.fail("AUTH_ARGON2_ITERATIONS", "must be > 0")
	}
	if a.Argon2.Parallelism == 0 {
		v.fail("AUTH_ARGON2_PARALLELISM", "must be > 0")
	}
	if a.Argon2.MaxConcurrent < 1 {
		v.fail("AUTH_ARGON2_MAX_CONCURRENT", "must be >= 1")
	}
	if a.InitialBalanceSeconds < 0 {
		v.fail("INITIAL_BALANCE_SECONDS", "must be >= 0, got %d", a.InitialBalanceSeconds)
	}
	v.positive("AUTH_ACCESS_TOKEN_TTL", a.AccessTokenTTL)
	v.positive("AUTH_REFRESH_TOKEN_TTL", a.RefreshTokenTTL)
	v.positive("AUTH_REFRESH_REUSE_GRACE", a.RefreshReuseGrace)
	v.positive("AUTH_PASSWORD_RESET_TTL", a.PasswordResetTTL)
	v.positive("WS_TICKET_TTL", a.WSTicketTTL)
}
