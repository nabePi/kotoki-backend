//go:build integration

package jobs_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/jobs"
	"github.com/nabePi/kotoki-backend/internal/testutil"
)

func advisoryLocks(t *testing.T, l jobs.PgLocker) int {
	t.Helper()
	var n int
	require.NoError(t, l.Pool.QueryRow(context.Background(), `SELECT count(*) FROM pg_locks WHERE locktype='advisory'`).Scan(&n))
	return n
}

func TestPgLocker_ExclusiveAcrossConnections(t *testing.T) {
	pool := testutil.NewPostgres(t)
	l := jobs.PgLocker{Pool: pool}
	const key = int64(7199001)
	ctx := context.Background()

	inside := make(chan struct{})
	release := make(chan struct{})
	var wg sync.WaitGroup
	var firstRan bool
	var firstErr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		firstRan, firstErr = l.RunExclusive(ctx, key, func(context.Context) error {
			close(inside)
			<-release
			return nil
		})
	}()
	<-inside

	var secondCalled bool
	ran, err := l.RunExclusive(ctx, key, func(context.Context) error { secondCalled = true; return nil })
	require.NoError(t, err)
	assert.False(t, ran, "the lock is held on another connection")
	assert.False(t, secondCalled)

	var otherRan bool
	ran, err = l.RunExclusive(ctx, key+1, func(context.Context) error { otherRan = true; return nil })
	require.NoError(t, err)
	assert.True(t, ran, "a different key does not block")
	assert.True(t, otherRan)

	close(release)
	wg.Wait()
	require.NoError(t, firstErr)
	assert.True(t, firstRan)
	assert.Equal(t, 0, advisoryLocks(t, l), "every lock is released on the server")

	ran, err = l.RunExclusive(ctx, key, func(context.Context) error { return nil })
	require.NoError(t, err)
	assert.True(t, ran, "the key can be taken again after the first run finished")
}

func TestPgLocker_ErrorFromFnStillReleases(t *testing.T) {
	pool := testutil.NewPostgres(t)
	l := jobs.PgLocker{Pool: pool}
	const key = int64(7199002)
	boom := errors.New("boom")

	ran, err := l.RunExclusive(context.Background(), key, func(context.Context) error { return boom })

	require.ErrorIs(t, err, boom)
	assert.True(t, ran)
	assert.Equal(t, 0, advisoryLocks(t, l))
	ran, err = l.RunExclusive(context.Background(), key, func(context.Context) error { return nil })
	require.NoError(t, err)
	assert.True(t, ran)
}

func TestPgLocker_CancelledContextStillReleases(t *testing.T) {
	pool := testutil.NewPostgres(t)
	l := jobs.PgLocker{Pool: pool}
	const key = int64(7199003)
	ctx, cancel := context.WithCancel(context.Background())

	ran, err := l.RunExclusive(ctx, key, func(context.Context) error { cancel(); return ctx.Err() })

	require.ErrorIs(t, err, context.Canceled)
	assert.True(t, ran)
	require.Eventually(t, func() bool { return advisoryLocks(t, l) == 0 }, 3*time.Second, 20*time.Millisecond,
		"unlock runs on a context detached from the cancelled one")
	ran, err = l.RunExclusive(context.Background(), key, func(context.Context) error { return nil })
	require.NoError(t, err)
	assert.True(t, ran)
}
