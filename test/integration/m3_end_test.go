//go:build integration

package integration_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/platform/clock"
	"github.com/nabePi/kotoki-backend/internal/postgres"
	"github.com/nabePi/kotoki-backend/internal/session"
)

// activate walks a created session to ACTIVE under a fresh fence and returns it.
func (e *sessEnv) activate(t *testing.T, key string, duration int) (session.Session, int64) {
	t.Helper()
	ctx := context.Background()
	res, err := e.svc.Create(ctx, e.input(key, duration))
	require.NoError(t, err)
	fence := e.claim(t, res.Session.ID, session.StatusReserved)
	_, err = e.svc.BeginStart(ctx, res.Session.ID, fence)
	require.NoError(t, err)
	got, err := e.svc.Activate(ctx, res.Session.ID, fence)
	require.NoError(t, err)
	return got, fence
}

func (e *sessEnv) countFor(t *testing.T, query string, id uuid.UUID) int {
	t.Helper()
	var n int
	require.NoError(t, e.pool.QueryRow(context.Background(), query, id).Scan(&n))
	return n
}

// TestFinalizeRuntimeVsSweeperRace: the runtime and the sweeper both finish the same ENDING session.
// One of them settles, the other finds it done; minutes and the outbox are written exactly once.
//
// The clock is fake and moves 30 s between Activate and BeginEnding: on the system clock that gap is
// under a millisecond, ComputeCharge then returns 0 and no CONSUME row is written at all, which
// is correct behaviour but makes the exactly-once assertion on CONSUME fail.
func TestFinalizeRuntimeVsSweeperRace(t *testing.T) {
	clk := clock.NewFake(time.Date(2026, 10, 14, 10, 0, 0, 0, time.UTC))
	e := newSessEnvClock(t, 3600, clk, nil)
	ctx := context.Background()
	for i := range 100 {
		s, fence := e.activate(t, "fin-race-"+uuid.NewString()[:8], 300)
		clk.Advance(30 * time.Second)
		_, err := e.svc.BeginEnding(ctx, s.ID, &fence, session.EndUserEnded, "", session.ActorRealtime)
		require.NoError(t, err)

		var wg sync.WaitGroup
		var rtErr, swErr error
		wg.Add(2)
		go func() { defer wg.Done(); _, rtErr = e.svc.Finalize(ctx, s.ID, &fence, session.ActorRealtime) }()
		go func() { defer wg.Done(); _, swErr = e.svc.Finalize(ctx, s.ID, nil, session.ActorSweeper) }()
		wg.Wait()

		require.NoError(t, swErr, "iteration %d: the sweeper never errors", i)
		if rtErr != nil {
			require.True(t, errors.Is(rtErr, session.ErrFenceLost), "iteration %d: %v", i, rtErr)
		}
		require.Equal(t, 1, e.countFor(t, `SELECT count(*) FROM balance_ledger WHERE session_id = $1 AND entry_type = 'CONSUME'`, s.ID), "iteration %d", i)
		require.Equal(t, 1, e.countFor(t, `SELECT count(*) FROM balance_ledger WHERE session_id = $1 AND entry_type = 'RELEASE'`, s.ID), "iteration %d", i)
		require.Equal(t, 1, e.countFor(t, `SELECT count(*) FROM event_outbox WHERE aggregate_id = $1 AND event_type = 'session.completed'`, s.ID), "iteration %d", i)
		require.Equal(t, 1, e.countFor(t, `SELECT count(*) FROM session_events WHERE session_id = $1 AND event_type = 'SESSION_COMPLETED'`, s.ID), "iteration %d", i)
	}
}

// TestHandoffThenSweeperRuleEExample10 is spec example 10: the runtime hands the session off at
// 10:03:00 and nobody resumes; at 10:03:31 rule (e) ends it, billing up to the handoff.
func TestHandoffThenSweeperRuleEExample10(t *testing.T) {
	clk := clock.NewFake(time.Date(2026, 10, 14, 10, 0, 0, 0, time.UTC))
	e := newSessEnvClock(t, 1800, clk, nil)
	ctx := context.Background()
	s, fence := e.activate(t, "example-10-key", 600)
	require.Equal(t, clk.Now(), *s.StartedAt)

	clk.Set(time.Date(2026, 10, 14, 10, 3, 0, 0, time.UTC))
	_, err := e.svc.MarkHandoff(ctx, s.ID, fence)
	require.NoError(t, err)

	clk.Set(time.Date(2026, 10, 14, 10, 3, 31, 0, time.UTC))
	repo := postgres.NewSessionRepo(e.pool)
	stale, err := repo.ListHandoffStale(ctx, clk.Now().Add(-30*time.Second), 200)
	require.NoError(t, err)
	require.Equal(t, []uuid.UUID{s.ID}, stale)

	_, err = e.svc.BeginEnding(ctx, s.ID, nil, session.EndReconnectTimeout, "handoff_not_resumed", session.ActorSweeper)
	require.NoError(t, err)
	got, err := e.svc.Finalize(ctx, s.ID, nil, session.ActorSweeper)
	require.NoError(t, err)
	require.Equal(t, session.StatusCompleted, got.Status)
	require.Equal(t, session.EndReconnectTimeout, *got.EndReason)
	require.Equal(t, 180, got.ChargedSeconds)
	require.Equal(t, time.Date(2026, 10, 14, 10, 3, 0, 0, time.UTC), got.EndingAt.UTC(), "ending_at = handoff_at")
	require.Equal(t, time.Date(2026, 10, 14, 10, 3, 31, 0, time.UTC), got.EndedAt.UTC())

	var available int64
	require.NoError(t, e.pool.QueryRow(ctx, `SELECT available_seconds FROM balance_accounts WHERE user_id = $1`, e.userID).Scan(&available))
	require.Equal(t, int64(1800-180), available)
}

// TestTakeoverOldFenceCannotWriteTurn: a second ClaimRuntime moves the fence and the first runtime
// is locked out of turns and events at once.
func TestTakeoverOldFenceCannotWriteTurn(t *testing.T) {
	e := newSessEnv(t, 1800, nil)
	ctx := context.Background()
	s, fence := e.activate(t, "takeover-key-0001", 300)
	turn := session.TurnStart{ID: uuid.Must(uuid.NewV7()), SessionID: s.ID, Sequence: 1, Generation: 1, CreatedAt: time.Now().UTC()}
	require.NoError(t, e.store.SessionStore().WithinTx(ctx, func(ctx context.Context, tx session.Tx) error { return tx.Turns().Insert(ctx, fence, turn) }))

	newFence, _, err := e.svc.ClaimRuntime(ctx, s.ID, []session.Status{session.StatusActive})
	require.NoError(t, err)
	require.Equal(t, fence+1, newFence)

	err = e.store.SessionStore().WithinTx(ctx, func(ctx context.Context, tx session.Tx) error {
		next := turn
		next.ID, next.Sequence = uuid.Must(uuid.NewV7()), 2
		return tx.Turns().Insert(ctx, fence, next)
	})
	require.ErrorIs(t, err, session.ErrFenceLost)
	err = e.store.SessionStore().WithinTx(ctx, func(ctx context.Context, tx session.Tx) error {
		return tx.Turns().Finalize(ctx, fence, turn.ID, session.TurnFinal{CreatedAt: turn.CreatedAt, Status: session.TurnCompleted, CompletedAt: time.Now().UTC()})
	})
	require.ErrorIs(t, err, session.ErrFenceLost)
	require.NoError(t, e.store.SessionStore().WithinTx(ctx, func(ctx context.Context, tx session.Tx) error {
		n, err := tx.Turns().FailInProgress(ctx, newFence, s.ID, nil)
		require.Equal(t, 1, n, "the new owner fails the turn the old one left open")
		return err
	}))
	_, err = e.svc.AddDisconnected(ctx, s.ID, fence, 3)
	require.ErrorIs(t, err, session.ErrFenceLost)
	_, err = e.svc.AddDisconnected(ctx, s.ID, newFence, 3)
	require.NoError(t, err)
}
