package config

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func b64(n int) string { return base64.StdEncoding.EncodeToString([]byte(strings.Repeat("s", n))) }

func authEnv(over map[string]string) map[string]string {
	m := map[string]string{
		"APP_ENV": "local", "DB_URL": "postgres://u:p@localhost:5432/kotoki?sslmode=disable", "REDIS_URL": "redis://localhost:6379/0",
		"AUTH_JWT_SECRET": b64(32), "PRIVACY_POLICY_VERSION": "2026-10-01",
	}
	for k, v := range over {
		if v == "" {
			delete(m, k)
			continue
		}
		m[k] = v
	}
	return m
}

func TestAuthConfigDefaults(t *testing.T) {
	a, err := loadAPI(authEnv(nil))
	require.NoError(t, err)
	require.Equal(t, 15*time.Minute, a.AccessTokenTTL)
	require.Equal(t, 720*time.Hour, a.RefreshTokenTTL)
	require.Equal(t, 10*time.Second, a.RefreshReuseGrace)
	require.Equal(t, 15*time.Minute, a.PasswordResetTTL)
	require.Equal(t, 30*time.Second, a.WSTicketTTL)
	require.Equal(t, "ws://localhost:8081", a.RealtimePublicURL)
	require.Equal(t, "log", a.Mail.Mode)
	require.Equal(t, uint32(19456), a.Argon2.MemoryKiB)
	require.Equal(t, uint32(2), a.Argon2.Iterations)
	require.Equal(t, uint8(1), a.Argon2.Parallelism)
	require.Equal(t, 8, a.Argon2.MaxConcurrent)
	require.Equal(t, int64(0), a.InitialBalanceSeconds)
	require.Equal(t, "kotoki-api", a.JWTIssuer)
	require.Equal(t, "kotoki", a.JWTAudience)
	require.True(t, a.RefreshCookieSecure)
	require.Equal(t, 587, a.Mail.SMTPPort)
	require.Equal(t, "2026-10-01", a.PrivacyPolicyVersion)

	secret, err := a.JWTSecretBytes()
	require.NoError(t, err)
	require.Len(t, secret, 32)
	prev, err := a.JWTPreviousSecretBytes()
	require.NoError(t, err)
	require.Nil(t, prev)
}

func TestAuthConfigSecretsNeverPrint(t *testing.T) {
	a, err := loadAPI(authEnv(map[string]string{"SMTP_PASSWORD": "hunter2"}))
	require.NoError(t, err)
	require.NotContains(t, a.AuthJWTSecret.String(), "c3Nz")
	require.NotContains(t, a.Mail.SMTPPassword.String(), "hunter2")
}

func TestAuthConfigValidate(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		want string
	}{
		{"missing secret", map[string]string{"AUTH_JWT_SECRET": ""}, "AUTH_JWT_SECRET"},
		{"short secret", map[string]string{"AUTH_JWT_SECRET": b64(31)}, "AUTH_JWT_SECRET"},
		{"secret not base64", map[string]string{"AUTH_JWT_SECRET": "not base64 !!!"}, "AUTH_JWT_SECRET"},
		{"short previous secret", map[string]string{"AUTH_JWT_PREVIOUS_SECRET": b64(5)}, "AUTH_JWT_PREVIOUS_SECRET"},
		{"missing terms", map[string]string{"PRIVACY_POLICY_VERSION": ""}, "PRIVACY_POLICY_VERSION"},
		{"bad terms", map[string]string{"PRIVACY_POLICY_VERSION": "has space"}, "PRIVACY_POLICY_VERSION"},
		{"prod log mail", map[string]string{"APP_ENV": "prod", "MAIL_MODE": "log"}, "MAIL_MODE"},
		{"staging insecure cookie", map[string]string{"APP_ENV": "staging", "AUTH_REFRESH_COOKIE_SECURE": "false"}, "AUTH_REFRESH_COOKIE_SECURE"},
		{"dev insecure cookie", map[string]string{"APP_ENV": "dev", "AUTH_REFRESH_COOKIE_SECURE": "false"}, "AUTH_REFRESH_COOKIE_SECURE"},
		{"prod reset url default", map[string]string{"APP_ENV": "prod", "MAIL_MODE": "smtp", "SMTP_HOST": "smtp.x", "PASSWORD_RESET_URL_BASE": ""}, "PASSWORD_RESET_URL_BASE"},
		{"prod reset url http", map[string]string{"APP_ENV": "prod", "MAIL_MODE": "smtp", "SMTP_HOST": "smtp.x", "PASSWORD_RESET_URL_BASE": "http://app.kotoki.id/reset"}, "PASSWORD_RESET_URL_BASE"},
		{"unknown mail mode", map[string]string{"MAIL_MODE": "pigeon"}, "MAIL_MODE"},
		{"smtp without host", map[string]string{"MAIL_MODE": "smtp"}, "SMTP_HOST"},
		{"bad trusted cidr", map[string]string{"TRUSTED_PROXY_CIDRS": "10.0.0.0/8,nope"}, "TRUSTED_PROXY_CIDRS"},
		{"zero argon memory", map[string]string{"AUTH_ARGON2_MEMORY_KIB": "0"}, "AUTH_ARGON2_MEMORY_KIB"},
		{"negative initial balance", map[string]string{"INITIAL_BALANCE_SECONDS": "-1"}, "INITIAL_BALANCE_SECONDS"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := loadAPI(authEnv(c.env))
			require.Error(t, err)
			require.Contains(t, err.Error(), c.want)
		})
	}

	_, err := loadAPI(authEnv(map[string]string{"AUTH_REFRESH_COOKIE_SECURE": "false"}))
	require.NoError(t, err, "an insecure cookie is fine in local")
	_, err = loadAPI(authEnv(map[string]string{"MAIL_MODE": "smtp", "SMTP_HOST": "smtp.example", "TRUSTED_PROXY_CIDRS": "10.0.0.0/8,fd00::/8"}))
	require.NoError(t, err)
}

func TestAuthConfigIsAPIOnly(t *testing.T) {
	env := authEnv(nil)
	delete(env, "AUTH_JWT_SECRET")
	delete(env, "PRIVACY_POLICY_VERSION")
	_, err := loadRealtime(env)
	require.NoError(t, err, "realtime does not need auth settings")
	_, err = loadWorker(env)
	require.NoError(t, err, "worker does not need auth settings")
}

func TestContentConfigDefaultsAndBounds(t *testing.T) {
	env := authEnv(nil)
	a, err := loadAPI(env)
	require.NoError(t, err)
	require.Equal(t, 30, a.ContentCacheTTLSeconds)
	require.Equal(t, 30*time.Second, a.CacheTTL())
	require.Equal(t, 180, a.RetentionTranscriptDays)

	for name, bad := range map[string]map[string]string{
		"ttl zero":       {"CONTENT_CACHE_TTL_SECONDS": "0"},
		"ttl too big":    {"CONTENT_CACHE_TTL_SECONDS": "7200"},
		"retention zero": {"RETENTION_TRANSCRIPT_DAYS": "0"},
	} {
		e := authEnv(bad)
		_, err := loadAPI(e)
		require.Error(t, err, name)
		for k := range bad {
			require.Contains(t, err.Error(), k, name)
		}
	}
	a, err = loadAPI(authEnv(map[string]string{"CONTENT_CACHE_TTL_SECONDS": "5", "RETENTION_TRANSCRIPT_DAYS": "90"}))
	require.NoError(t, err)
	require.Equal(t, 5*time.Second, a.CacheTTL())
	require.Equal(t, 90, a.RetentionTranscriptDays)
}
