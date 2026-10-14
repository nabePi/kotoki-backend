package auth

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/platform/errs"
)

func defaultArgon2() Argon2Config {
	return Argon2Config{MemoryKiB: 19456, Iterations: 2, Parallelism: 1, MaxConcurrent: 8, MaxWait: 2 * time.Second}
}

func TestArgon2PHCFormat(t *testing.T) {
	h := NewArgon2Hasher(defaultArgon2())
	got, err := h.Hash(context.Background(), "correct horse battery")
	require.NoError(t, err)
	require.Regexp(t, `^\$argon2id\$v=19\$m=19456,t=2,p=1\$[A-Za-z0-9+/]{22}\$[A-Za-z0-9+/]{43}$`, got)
}

func TestArgon2VerifyRoundTrip(t *testing.T) {
	cfg := defaultArgon2()
	cfg.MemoryKiB = 64
	h := NewArgon2Hasher(cfg)
	ctx := context.Background()
	enc, err := h.Hash(ctx, "correct horse battery")
	require.NoError(t, err)

	ok, err := h.Verify(ctx, "correct horse battery", enc)
	require.NoError(t, err)
	require.True(t, ok)
	ok, err = h.Verify(ctx, "wrong password!!", enc)
	require.NoError(t, err)
	require.False(t, ok)

	for _, bad := range []string{"!", "", "$argon2id$v=19$m=64,t=2,p=1$onlyonepart", "$argon2i$v=19$m=64,t=2,p=1$AAAA$AAAA", "$argon2id$v=19$m=x,t=2,p=1$AAAA$AAAA"} {
		ok, err := h.Verify(ctx, "correct horse battery", bad)
		require.NoError(t, err, bad)
		require.False(t, ok, bad)
	}
}

func TestArgon2NeedsRehash(t *testing.T) {
	small := defaultArgon2()
	small.MemoryKiB = 64
	enc, err := NewArgon2Hasher(small).Hash(context.Background(), "correct horse battery")
	require.NoError(t, err)

	bigger := small
	bigger.MemoryKiB = 32768
	require.True(t, NewArgon2Hasher(bigger).NeedsRehash(enc))
	require.False(t, NewArgon2Hasher(small).NeedsRehash(enc))
	require.True(t, NewArgon2Hasher(small).NeedsRehash("garbage"))
}

func TestArgon2ConcurrencyLimit(t *testing.T) {
	cfg := defaultArgon2()
	cfg.MaxConcurrent, cfg.MaxWait = 1, time.Millisecond
	h := NewArgon2Hasher(cfg)
	started, release := make(chan struct{}), make(chan struct{})
	h.derive = func(pw, salt []byte) []byte {
		close(started)
		<-release
		return make([]byte, 32)
	}
	done := make(chan error, 1)
	go func() { _, err := h.Hash(context.Background(), "correct horse battery"); done <- err }()
	<-started

	_, err := h.Hash(context.Background(), "another password!!")
	require.Equal(t, errs.CodeServiceUnavailable, errs.CodeOf(err))

	close(release)
	require.NoError(t, <-done)
	h.derive = func(pw, salt []byte) []byte { return make([]byte, 32) }
	_, err = h.Hash(context.Background(), "another password!!")
	require.NoError(t, err)
}
