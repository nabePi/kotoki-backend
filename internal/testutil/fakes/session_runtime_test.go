package fakes_test

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/conversation"
	"github.com/nabePi/kotoki-backend/internal/platform/clock"
	"github.com/nabePi/kotoki-backend/internal/session"
	"github.com/nabePi/kotoki-backend/internal/testutil/fakes"
)

var _ conversation.SessionRuntime = (*fakes.SessionRuntime)(nil)

func newRuntime(t *testing.T, duration int) (*fakes.SessionRuntime, *clock.Fake, uuid.UUID) {
	t.Helper()
	clk := clock.NewFake(time.Date(2026, 10, 15, 9, 0, 0, 0, time.UTC))
	rt := fakes.NewSessionRuntime(clk)
	id := uuid.Must(uuid.NewV7())
	rt.Seed(session.RuntimeRecord{ID: id, UserID: uuid.New(), Status: session.StatusReserved, DurationSeconds: duration})
	return rt, clk, id
}

func TestFakeSessionRuntime(t *testing.T) {
	ctx := t.Context()

	t.Run("happy path and settlement", func(t *testing.T) {
		rt, clk, id := newRuntime(t, 300)
		fence, rec, err := rt.ClaimRuntime(ctx, id, []session.Status{session.StatusReserved})
		require.NoError(t, err)
		require.EqualValues(t, 1, fence)
		require.Equal(t, fence, rec.RuntimeFence)
		rec, err = rt.MarkStarting(ctx, id, fence)
		require.NoError(t, err)
		require.Equal(t, session.StatusStarting, rec.Status)
		started, expires, err := rt.Activate(ctx, id, fence)
		require.NoError(t, err)
		require.Equal(t, started.Add(300*time.Second), expires)
		clk.Advance(100*time.Second + 400*time.Millisecond) // ceil → 101
		require.NoError(t, rt.AddDisconnected(ctx, id, fence, 5))
		require.NoError(t, rt.AddDisconnected(ctx, id, fence, 7))
		require.NoError(t, rt.BeginEnding(ctx, id, fence, session.EndUserEnded))
		require.NoError(t, rt.BeginEnding(ctx, id, fence, session.EndUserEnded)) // no-op
		clk.Advance(time.Minute)                                                 // after ENDING: not charged
		s, err := rt.Complete(ctx, id, fence)
		require.NoError(t, err)
		require.Equal(t, 101-12, s.ChargedSeconds)
		require.Equal(t, 12, s.DisconnectedSeconds)
		require.Equal(t, session.EndUserEnded, s.EndReason)
		again, err := rt.Complete(ctx, id, fence)
		require.NoError(t, err)
		require.Equal(t, s, again)
		got, err := rt.GetForAttach(ctx, id)
		require.NoError(t, err)
		require.Equal(t, session.StatusCompleted, got.Status)
	})

	t.Run("charge capped by duration", func(t *testing.T) {
		rt, clk, id := newRuntime(t, 60)
		fence, _, _ := rt.ClaimRuntime(ctx, id, []session.Status{session.StatusReserved})
		_, _ = rt.MarkStarting(ctx, id, fence)
		_, _, _ = rt.Activate(ctx, id, fence)
		clk.Advance(200 * time.Second)
		require.NoError(t, rt.BeginEnding(ctx, id, fence, session.EndDurationElapsed))
		s, err := rt.Complete(ctx, id, fence)
		require.NoError(t, err)
		require.Equal(t, 60, s.ChargedSeconds)
	})

	t.Run("fence lost and claim bump", func(t *testing.T) {
		rt, _, id := newRuntime(t, 300)
		f1, _, _ := rt.ClaimRuntime(ctx, id, []session.Status{session.StatusReserved})
		_, _ = rt.MarkStarting(ctx, id, f1)
		_, _, _ = rt.Activate(ctx, id, f1)
		f2, _, err := rt.ClaimRuntime(ctx, id, []session.Status{session.StatusActive})
		require.NoError(t, err)
		require.Greater(t, f2, f1)
		require.ErrorIs(t, rt.AddDisconnected(ctx, id, f1, 1), session.ErrFenceLost)
		require.ErrorIs(t, rt.BeginEnding(ctx, id, f1, session.EndUserEnded), session.ErrFenceLost)
		require.ErrorIs(t, rt.MarkHandoff(ctx, id, f1), session.ErrFenceLost)
		require.NoError(t, rt.MarkHandoff(ctx, id, f2))
	})

	t.Run("invalid transitions and claim filter", func(t *testing.T) {
		rt, _, id := newRuntime(t, 300)
		_, _, err := rt.ClaimRuntime(ctx, id, []session.Status{session.StatusActive})
		require.ErrorIs(t, err, session.ErrInvalidTransition)
		f, _, _ := rt.ClaimRuntime(ctx, id, []session.Status{session.StatusReserved})
		_, _, err = rt.Activate(ctx, id, f) // RESERVED → ACTIVE not allowed
		require.ErrorIs(t, err, session.ErrInvalidTransition)
		require.ErrorIs(t, rt.BeginEnding(ctx, id, f, session.EndUserEnded), session.ErrInvalidTransition)
		_, err = rt.GetForAttach(ctx, uuid.New())
		require.ErrorIs(t, err, session.ErrNotFound)
	})

	t.Run("fail start and fail", func(t *testing.T) {
		rt, _, id := newRuntime(t, 300)
		f, _, _ := rt.ClaimRuntime(ctx, id, []session.Status{session.StatusReserved})
		_, _ = rt.MarkStarting(ctx, id, f)
		s, err := rt.FailStart(ctx, id, f, session.EndProviderFailure)
		require.NoError(t, err)
		require.Zero(t, s.ChargedSeconds)

		rt2, _, id2 := newRuntime(t, 300)
		f2, _, _ := rt2.ClaimRuntime(ctx, id2, []session.Status{session.StatusReserved})
		_, _ = rt2.MarkStarting(ctx, id2, f2)
		_, _, _ = rt2.Activate(ctx, id2, f2)
		require.NoError(t, rt2.BeginEnding(ctx, id2, f2, session.EndServerError))
		_, err = rt2.Fail(ctx, id2, f2, session.EndServerError)
		require.NoError(t, err)
		got, _ := rt2.GetForAttach(ctx, id2)
		require.Equal(t, session.StatusFailed, got.Status)
	})

	t.Run("FailNext", func(t *testing.T) {
		rt, _, id := newRuntime(t, 300)
		boom := errors.New("boom")
		rt.FailNext("ClaimRuntime", boom)
		_, _, err := rt.ClaimRuntime(ctx, id, []session.Status{session.StatusReserved})
		require.ErrorIs(t, err, boom)
		_, _, err = rt.ClaimRuntime(ctx, id, []session.Status{session.StatusReserved})
		require.NoError(t, err)
	})
}

func TestFakeSessionRuntimeSeedKeepsDisconnected(t *testing.T) {
	ctx := t.Context()
	rt := fakes.NewSessionRuntime(clock.NewFake(time.Date(2026, 10, 15, 9, 0, 0, 0, time.UTC)))
	id := uuid.Must(uuid.NewV7())
	rt.Seed(session.RuntimeRecord{ID: id, UserID: uuid.New(), Status: session.StatusActive, RuntimeFence: 3, DisconnectedSeconds: 5})
	require.Equal(t, 5, rt.Disconnected(id), "the seeded value is the starting point")
	require.NoError(t, rt.AddDisconnected(ctx, id, 3, 4))
	require.Equal(t, 9, rt.Disconnected(id))
	rec, err := rt.GetForAttach(ctx, id)
	require.NoError(t, err)
	require.Equal(t, 9, rec.DisconnectedSeconds, "the record and the counter agree")
}
