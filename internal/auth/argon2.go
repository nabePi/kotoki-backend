package auth

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	"golang.org/x/crypto/argon2"

	"github.com/nabePi/kotoki-backend/internal/platform/errs"
)

const (
	argonSaltLen = 16
	argonKeyLen  = 32
)

// Argon2Config sets the argon2id cost and the per-pod concurrency limit (spec 3.4).
type Argon2Config struct {
	MemoryKiB     uint32
	Iterations    uint32
	Parallelism   uint8
	MaxConcurrent int
	MaxWait       time.Duration
}

// Argon2Hasher implements PasswordHasher with argon2id and PHC strings.
type Argon2Hasher struct {
	cfg Argon2Config
	sem chan struct{}
	// derive computes the key with the configured parameters; tests replace it.
	derive func(pw, salt []byte) []byte
}

var _ PasswordHasher = (*Argon2Hasher)(nil)

// NewArgon2Hasher returns a hasher; at most cfg.MaxConcurrent derivations run at once.
func NewArgon2Hasher(cfg Argon2Config) *Argon2Hasher {
	if cfg.MaxConcurrent < 1 {
		cfg.MaxConcurrent = 1
	}
	h := &Argon2Hasher{cfg: cfg, sem: make(chan struct{}, cfg.MaxConcurrent)}
	h.derive = func(pw, salt []byte) []byte {
		return argon2.IDKey(pw, salt, cfg.Iterations, cfg.MemoryKiB, cfg.Parallelism, argonKeyLen)
	}
	return h
}

func (h *Argon2Hasher) acquire(ctx context.Context) error {
	timer := time.NewTimer(h.cfg.MaxWait)
	defer timer.Stop()
	select {
	case h.sem <- struct{}{}:
		return nil
	case <-timer.C:
		return errs.New(errs.CodeServiceUnavailable, "password hashing is busy")
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (h *Argon2Hasher) release() { <-h.sem }

// Hash returns the PHC encoding of plaintext with a fresh random salt.
func (h *Argon2Hasher) Hash(ctx context.Context, plaintext string) (string, error) {
	if err := h.acquire(ctx); err != nil {
		return "", err
	}
	defer h.release()
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", errs.Wrap(err, errs.CodeInternalError, "salt generation failed")
	}
	key := h.derive([]byte(plaintext), salt)
	enc := base64.RawStdEncoding
	return fmt.Sprintf("$argon2id$v=19$m=%d,t=%d,p=%d$%s$%s",
		h.cfg.MemoryKiB, h.cfg.Iterations, h.cfg.Parallelism, enc.EncodeToString(salt), enc.EncodeToString(key)), nil
}

type phc struct {
	m, t    uint32
	p       uint8
	salt, k []byte
}

func parsePHC(encoded string) (phc, bool) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" || parts[2] != "v=19" {
		return phc{}, false
	}
	var out phc
	var p uint32
	if n, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &out.m, &out.t, &p); err != nil || n != 3 || p > 255 {
		return phc{}, false
	}
	out.p = uint8(p)
	var err1, err2 error
	out.salt, err1 = base64.RawStdEncoding.DecodeString(parts[4])
	out.k, err2 = base64.RawStdEncoding.DecodeString(parts[5])
	if err1 != nil || err2 != nil || len(out.salt) == 0 || len(out.k) != argonKeyLen {
		return phc{}, false
	}
	return out, true
}

// Verify reports whether plaintext matches encoded. Malformed encodings are
// simply a non-match, never an error.
func (h *Argon2Hasher) Verify(ctx context.Context, plaintext, encoded string) (bool, error) {
	parsed, ok := parsePHC(encoded)
	if !ok {
		return false, nil
	}
	if err := h.acquire(ctx); err != nil {
		return false, err
	}
	defer h.release()
	var key []byte
	if parsed.m == h.cfg.MemoryKiB && parsed.t == h.cfg.Iterations && parsed.p == h.cfg.Parallelism {
		key = h.derive([]byte(plaintext), parsed.salt)
	} else {
		key = argon2.IDKey([]byte(plaintext), parsed.salt, parsed.t, parsed.m, parsed.p, argonKeyLen)
	}
	return subtle.ConstantTimeCompare(key, parsed.k) == 1, nil
}

// NeedsRehash reports whether encoded was produced with other parameters (or is unparsable).
func (h *Argon2Hasher) NeedsRehash(encoded string) bool {
	parsed, ok := parsePHC(encoded)
	if !ok {
		return true
	}
	return parsed.m != h.cfg.MemoryKiB || parsed.t != h.cfg.Iterations || parsed.p != h.cfg.Parallelism
}
