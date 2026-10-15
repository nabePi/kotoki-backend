package auth

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	"github.com/nabePi/kotoki-backend/internal/platform/idgen"
)

const (
	minSecretLen = 32
	jwtLeeway    = 30 * time.Second
)

// JWTConfig configures access-token signing and verification (spec 3.6).
// Secret and PreviousSecret are raw bytes (already base64-decoded).
type JWTConfig struct {
	Secret, PreviousSecret []byte
	Issuer, Audience       string
	TTL                    time.Duration
}

// JWTIssuer implements TokenIssuer with HS256 tokens carrying a key id.
type JWTIssuer struct {
	cfg  JWTConfig
	ids  idgen.Generator
	keys map[string][]byte // kid → secret (active and previous)
	kid  string            // active kid
}

var _ TokenIssuer = (*JWTIssuer)(nil)

func keyID(secret []byte) string {
	sum := sha256.Sum256(secret)
	return hex.EncodeToString(sum[:])[:8]
}

// NewJWTIssuer validates cfg: secrets must be at least 32 bytes.
func NewJWTIssuer(cfg JWTConfig, ids idgen.Generator) (*JWTIssuer, error) {
	if len(cfg.Secret) < minSecretLen {
		return nil, fmt.Errorf("auth: JWT secret must be at least %d bytes", minSecretLen)
	}
	j := &JWTIssuer{cfg: cfg, ids: ids, keys: map[string][]byte{}, kid: keyID(cfg.Secret)}
	j.keys[j.kid] = cfg.Secret
	if len(cfg.PreviousSecret) > 0 {
		if len(cfg.PreviousSecret) < minSecretLen {
			return nil, fmt.Errorf("auth: previous JWT secret must be at least %d bytes", minSecretLen)
		}
		j.keys[keyID(cfg.PreviousSecret)] = cfg.PreviousSecret
	}
	return j, nil
}

type accessClaims struct {
	Role string `json:"role"`
	Fam  string `json:"fam"`
	jwt.RegisteredClaims
}

// IssueAccess signs an access token valid from now for cfg.TTL.
func (j *JWTIssuer) IssueAccess(p AccessSubject, now time.Time) (string, time.Time, error) {
	exp := now.Add(j.cfg.TTL)
	c := accessClaims{
		Role: string(p.Role),
		Fam:  p.FamilyID.String(),
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    j.cfg.Issuer,
			Audience:  jwt.ClaimStrings{j.cfg.Audience},
			Subject:   p.UserID.String(),
			ID:        j.ids.New().String(),
			IssuedAt:  jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(exp),
		},
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, c)
	tok.Header["kid"] = j.kid
	signed, err := tok.SignedString(j.cfg.Secret)
	if err != nil {
		return "", time.Time{}, err
	}
	return signed, exp.Truncate(time.Second), nil
}

// ParseAccess verifies token as of now and returns its claims.
func (j *JWTIssuer) ParseAccess(token string, now time.Time) (Claims, error) {
	var c accessClaims
	parser := jwt.NewParser(
		jwt.WithValidMethods([]string{"HS256"}),
		jwt.WithTimeFunc(func() time.Time { return now }),
		jwt.WithLeeway(jwtLeeway),
		jwt.WithIssuer(j.cfg.Issuer),
		jwt.WithAudience(j.cfg.Audience),
		jwt.WithExpirationRequired(),
	)
	var kid string
	_, err := parser.ParseWithClaims(token, &c, func(t *jwt.Token) (any, error) {
		id, _ := t.Header["kid"].(string)
		secret, ok := j.keys[id]
		if !ok {
			return nil, errors.New("unknown kid")
		}
		kid = id
		return secret, nil
	})
	if err != nil {
		if errors.Is(err, jwt.ErrTokenExpired) {
			return Claims{}, ErrTokenExpired
		}
		return Claims{}, ErrTokenInvalid
	}
	user, err1 := uuid.Parse(c.Subject)
	fam, err2 := uuid.Parse(c.Fam)
	jti, err3 := uuid.Parse(c.ID)
	role := Role(c.Role)
	if err1 != nil || err2 != nil || err3 != nil || (role != RoleUser && role != RoleAdmin) ||
		c.IssuedAt == nil || c.NotBefore == nil || c.ExpiresAt == nil {
		return Claims{}, ErrTokenInvalid
	}
	return Claims{
		UserID: user, Role: role, FamilyID: fam, JTI: jti,
		IssuedAt: c.IssuedAt.UTC(), NotBefore: c.NotBefore.UTC(), ExpiresAt: c.ExpiresAt.UTC(), KeyID: kid,
	}, nil
}
