//go:build integration

package integration_test

import (
	"context"
	"errors"
	"math/rand/v2"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/platform/errs"
	"github.com/nabePi/kotoki-backend/internal/postgres"
	"github.com/nabePi/kotoki-backend/internal/session"
)

// claim is what the runtime does first: raise the fence and keep it.
func (e *sessEnv) claim(t *testing.T, id uuid.UUID, allowed ...session.Status) int64 {
	t.Helper()
	var fence int64
	require.NoError(t, e.store.SessionStore().WithinTx(context.Background(), func(ctx context.Context, tx session.Tx) error {
		s, err := tx.Sessions().Claim(ctx, id, allowed)
		fence = s.RuntimeFence
		return err
	}))
	return fence
}

func (e *sessEnv) create(t *testing.T, key string) session.Session {
	t.Helper()
	res, err := e.svc.Create(context.Background(), e.input(key, 300))
	require.NoError(t, err)
	return res.Session
}

// TestCancelVsBeginStartRace: the user cancels while the runtime starts. Whoever takes the lock first
// wins, the other one is told it lost, and the books balance either way.
func TestCancelVsBeginStartRace(t *testing.T) {
	e := newSessEnv(t, 600, nil)
	ctx := context.Background()
	cancelWins, startWins := 0, 0
	for i := range 50 {
		s := e.create(t, "race-key-"+uuid.NewString()[:8])
		fence := e.claim(t, s.ID, session.StatusReserved)

		var wg sync.WaitGroup
		var cancelErr, startErr error
		wg.Add(2)
		// random head starts of up to 2 ms make both orders happen
		d1, d2 := time.Duration(rand.IntN(2000))*time.Microsecond, time.Duration(rand.IntN(2000))*time.Microsecond
		go func() { defer wg.Done(); time.Sleep(d1); _, cancelErr = e.svc.Cancel(ctx, e.userID, s.ID) }()
		go func() { defer wg.Done(); time.Sleep(d2); _, startErr = e.svc.BeginStart(ctx, s.ID, fence) }()
		wg.Wait()

		switch {
		case cancelErr == nil && startErr != nil:
			require.ErrorIs(t, startErr, session.ErrInvalidTransition, "iteration %d", i)
			cancelWins++
		case cancelErr != nil && startErr == nil:
			require.Equal(t, errs.CodeSessionNotCancellable, errs.CodeOf(cancelErr), "iteration %d", i)
			startWins++
			_, err := e.svc.FailStart(ctx, s.ID, &fence, session.EndServerError, "race_cleanup", session.ActorRealtime)
			require.NoError(t, err)
		default:
			t.Fatalf("iteration %d: exactly one must win, cancel=%v start=%v", i, cancelErr, startErr)
		}
		var open int
		require.NoError(t, e.pool.QueryRow(ctx, `SELECT count(*) FROM sessions WHERE user_id = $1 AND status IN ('RESERVED','STARTING','ACTIVE','ENDING')`, e.userID).Scan(&open))
		require.Zero(t, open)
	}
	t.Logf("cancel won %d times, start won %d times", cancelWins, startWins)
	var available int64
	require.NoError(t, e.pool.QueryRow(ctx, `SELECT available_seconds FROM balance_accounts WHERE user_id = $1`, e.userID).Scan(&available))
	require.Equal(t, int64(600), available, "every reservation went back")
}

func TestCancelInTxUsedByAdminTx(t *testing.T) {
	e := newSessEnv(t, 600, nil)
	ctx := context.Background()
	s := e.create(t, "admin-tx-key-0001")
	require.NoError(t, e.store.WithinTx(ctx, func(ctx context.Context, tx *postgres.Tx) error {
		got, err := e.svc.CancelInTx(ctx, tx, s.ID, session.ActorAdmin)
		require.NoError(t, err)
		require.Equal(t, session.StatusCancelled, got.Status)
		return nil
	}))
	var status string
	require.NoError(t, e.pool.QueryRow(ctx, `SELECT status FROM sessions WHERE id = $1`, s.ID).Scan(&status))
	require.Equal(t, "CANCELLED", status)

	// a later error in the caller's transaction undoes the cancellation too
	s2 := e.create(t, "admin-tx-key-0002")
	boom := errors.New("boom")
	err := e.store.WithinTx(ctx, func(ctx context.Context, tx *postgres.Tx) error {
		if _, err := e.svc.CancelInTx(ctx, tx, s2.ID, session.ActorAdmin); err != nil {
			return err
		}
		return boom
	})
	require.ErrorIs(t, err, boom)
	require.NoError(t, e.pool.QueryRow(ctx, `SELECT status FROM sessions WHERE id = $1`, s2.ID).Scan(&status))
	require.Equal(t, "RESERVED", status)
	var held int
	require.NoError(t, e.pool.QueryRow(ctx, `SELECT count(*) FROM balance_reservations WHERE session_id = $1 AND status = 'HELD'`, s2.ID).Scan(&held))
	require.Equal(t, 1, held)
	_, err = e.svc.Cancel(ctx, e.userID, s2.ID)
	require.NoError(t, err)
}
