package auth

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/platform/idgen"
)

var (
	secretA = []byte(strings.Repeat("a", 32))
	secretB = []byte(strings.Repeat("b", 32))
	t0      = time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
)

func newIssuer(t *testing.T, cfg JWTConfig) *JWTIssuer {
	t.Helper()
	if cfg.Issuer == "" {
		cfg.Issuer = "kotoki-api"
	}
	if cfg.Audience == "" {
		cfg.Audience = "kotoki"
	}
	if cfg.TTL == 0 {
		cfg.TTL = 15 * time.Minute
	}
	j, err := NewJWTIssuer(cfg, idgen.NewSequence(t))
	require.NoError(t, err)
	return j
}

func subject() AccessSubject {
	return AccessSubject{UserID: uuid.MustParse("0192a3b4-0000-7000-8000-000000000001"), Role: RoleAdmin,
		FamilyID: uuid.MustParse("0192a3b4-0000-7000-8000-000000000002")}
}

func TestJWTRoundTrip(t *testing.T) {
	j := newIssuer(t, JWTConfig{Secret: secretA})
	tok, exp, err := j.IssueAccess(subject(), t0)
	require.NoError(t, err)
	require.Equal(t, t0.Add(15*time.Minute), exp)

	c, err := j.ParseAccess(tok, t0.Add(time.Minute))
	require.NoError(t, err)
	require.Equal(t, subject().UserID, c.UserID)
	require.Equal(t, RoleAdmin, c.Role)
	require.Equal(t, subject().FamilyID, c.FamilyID)
	require.Equal(t, exp, c.ExpiresAt)
	require.Equal(t, 15*time.Minute, c.ExpiresAt.Sub(c.IssuedAt))
	require.NotEqual(t, uuid.Nil, c.JTI)

	sum := sha256.Sum256(secretA)
	wantKid := hex.EncodeToString(sum[:])[:8]
	require.Equal(t, wantKid, c.KeyID)
	parsed, _, err := jwt.NewParser().ParseUnverified(tok, jwt.MapClaims{})
	require.NoError(t, err)
	require.Equal(t, wantKid, parsed.Header["kid"])
	require.Equal(t, "HS256", parsed.Header["alg"])
}

func TestJWTUniqueJTI(t *testing.T) {
	j := newIssuer(t, JWTConfig{Secret: secretA})
	a, _, _ := j.IssueAccess(subject(), t0)
	b, _, _ := j.IssueAccess(subject(), t0)
	ca, err := j.ParseAccess(a, t0)
	require.NoError(t, err)
	cb, err := j.ParseAccess(b, t0)
	require.NoError(t, err)
	require.NotEqual(t, ca.JTI, cb.JTI)
}

func TestJWTRejectsAlgNone(t *testing.T) {
	j := newIssuer(t, JWTConfig{Secret: secretA})
	tok := jwt.NewWithClaims(jwt.SigningMethodNone, jwt.MapClaims{
		"iss": "kotoki-api", "aud": "kotoki", "sub": subject().UserID.String(), "role": "ADMIN",
		"fam": subject().FamilyID.String(), "jti": uuid.NewString(),
		"iat": t0.Unix(), "nbf": t0.Unix(), "exp": t0.Add(time.Hour).Unix(),
	})
	raw, err := tok.SignedString(jwt.UnsafeAllowNoneSignatureType)
	require.NoError(t, err)
	_, err = j.ParseAccess(raw, t0)
	require.ErrorIs(t, err, ErrTokenInvalid)
}

func TestJWTUnknownKid(t *testing.T) {
	other := newIssuer(t, JWTConfig{Secret: secretB})
	tok, _, err := other.IssueAccess(subject(), t0)
	require.NoError(t, err)
	_, err = newIssuer(t, JWTConfig{Secret: secretA}).ParseAccess(tok, t0)
	require.ErrorIs(t, err, ErrTokenInvalid)
}

func TestJWTPreviousSecret(t *testing.T) {
	old := newIssuer(t, JWTConfig{Secret: secretA})
	tok, _, err := old.IssueAccess(subject(), t0)
	require.NoError(t, err)

	rotated := newIssuer(t, JWTConfig{Secret: secretB, PreviousSecret: secretA})
	_, err = rotated.ParseAccess(tok, t0)
	require.NoError(t, err)

	noPrev := newIssuer(t, JWTConfig{Secret: secretB})
	_, err = noPrev.ParseAccess(tok, t0)
	require.ErrorIs(t, err, ErrTokenInvalid)

	// the previous secret only verifies: new tokens are always signed with the active one
	fresh, _, err := rotated.IssueAccess(subject(), t0)
	require.NoError(t, err)
	_, err = noPrev.ParseAccess(fresh, t0)
	require.NoError(t, err)
}

func TestJWTExpiryLeeway(t *testing.T) {
	j := newIssuer(t, JWTConfig{Secret: secretA})
	tok, exp, err := j.IssueAccess(subject(), t0)
	require.NoError(t, err)

	_, err = j.ParseAccess(tok, exp.Add(29*time.Second))
	require.NoError(t, err)
	_, err = j.ParseAccess(tok, exp.Add(31*time.Second))
	require.ErrorIs(t, err, ErrTokenExpired)

	_, err = j.ParseAccess(tok, t0.Add(-29*time.Second)) // nbf = t0
	require.NoError(t, err)
	_, err = j.ParseAccess(tok, t0.Add(-31*time.Second))
	require.ErrorIs(t, err, ErrTokenInvalid)
}

func TestJWTWrongIssuerOrAudience(t *testing.T) {
	tok, _, err := newIssuer(t, JWTConfig{Secret: secretA, Issuer: "someone-else"}).IssueAccess(subject(), t0)
	require.NoError(t, err)
	_, err = newIssuer(t, JWTConfig{Secret: secretA}).ParseAccess(tok, t0)
	require.ErrorIs(t, err, ErrTokenInvalid)

	tok, _, err = newIssuer(t, JWTConfig{Secret: secretA, Audience: "other"}).IssueAccess(subject(), t0)
	require.NoError(t, err)
	_, err = newIssuer(t, JWTConfig{Secret: secretA}).ParseAccess(tok, t0)
	require.ErrorIs(t, err, ErrTokenInvalid)
}

func TestJWTMalformedAndBadClaims(t *testing.T) {
	j := newIssuer(t, JWTConfig{Secret: secretA})
	_, err := j.ParseAccess("not.a.jwt", t0)
	require.ErrorIs(t, err, ErrTokenInvalid)

	sum := sha256.Sum256(secretA)
	bad := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"iss": "kotoki-api", "aud": "kotoki", "sub": "not-a-uuid", "role": "ROOT",
		"fam": "x", "jti": "y", "iat": t0.Unix(), "nbf": t0.Unix(), "exp": t0.Add(time.Hour).Unix(),
	})
	bad.Header["kid"] = hex.EncodeToString(sum[:])[:8]
	raw, err := bad.SignedString(secretA)
	require.NoError(t, err)
	_, err = j.ParseAccess(raw, t0)
	require.ErrorIs(t, err, ErrTokenInvalid)
}

func TestNewJWTIssuerShortSecret(t *testing.T) {
	_, err := NewJWTIssuer(JWTConfig{Secret: make([]byte, 31), Issuer: "i", Audience: "a", TTL: time.Minute}, idgen.NewV7())
	require.Error(t, err)
	_, err = NewJWTIssuer(JWTConfig{Secret: secretA, PreviousSecret: make([]byte, 5), Issuer: "i", Audience: "a", TTL: time.Minute}, idgen.NewV7())
	require.Error(t, err)
}
