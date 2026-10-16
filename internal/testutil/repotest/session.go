package repotest

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/session"
)

// SessionHarness is one fresh session store plus a way to build sessions it accepts (PostgreSQL
// needs real users and content rows, a fake does not).
type SessionHarness struct {
	Store session.Store
	// Build returns an uninserted RESERVED session; owner nil means a fresh user.
	Build func(t *testing.T, owner *uuid.UUID) session.Session
}

// RunSessionRepoSuite runs the contract of session.Repository, TurnRepository and EventRepository
// against mk's store: CAS and fence rules, Claim, AddDisconnected, fenced turn and event writes.
func RunSessionRepoSuite(t *testing.T, mk func(t *testing.T) SessionHarness) {
	t.Helper()
	Each(t, mk,
		Case[SessionHarness]{"insert and read back", sessionInsertRoundTrip},
		Case[SessionHarness]{"second open session and duplicate idempotency key", sessionUniqueness},
		Case[SessionHarness]{"CAS guards status and fence and bumps version", sessionCAS},
		Case[SessionHarness]{"claim raises the fence without touching version", sessionClaim},
		Case[SessionHarness]{"add disconnected drops the part after expires_at", sessionAddDisconnected},
		Case[SessionHarness]{"mark handoff needs ACTIVE and the fence", sessionMarkHandoff},
		Case[SessionHarness]{"turns are fenced", sessionTurnsFenced},
		Case[SessionHarness]{"fail in progress and counters", sessionTurnsFailInProgress},
		Case[SessionHarness]{"events are fenced", sessionEventsFenced},
		Case[SessionHarness]{"config snapshot never changes", sessionSnapshotImmutable},
	)
}

var sessionNow = func() time.Time { return time.Now().UTC().Truncate(time.Millisecond) }

func within(t *testing.T, h SessionHarness, fn func(ctx context.Context, tx session.Tx) error) {
	t.Helper()
	require.NoError(t, h.Store.WithinTx(context.Background(), fn))
}

// insertActive inserts a session and walks it to ACTIVE under fence 1 (what the runtime does).
func insertActive(t *testing.T, h SessionHarness) (session.Session, int64) {
	t.Helper()
	s := h.Build(t, nil)
	now := sessionNow()
	var out session.Session
	within(t, h, func(ctx context.Context, tx session.Tx) error {
		r := tx.Sessions()
		if err := r.Insert(ctx, s); err != nil {
			return err
		}
		claimed, err := r.Claim(ctx, s.ID, []session.Status{session.StatusReserved})
		if err != nil {
			return err
		}
		fence := claimed.RuntimeFence
		if _, ok, err := r.CAS(ctx, s.ID, session.StatusReserved, &fence, session.Patch{To: session.StatusStarting, At: now, StartingAt: &now}); err != nil || !ok {
			require.True(t, ok)
			return err
		}
		exp := now.Add(time.Duration(s.DurationSeconds) * time.Second)
		var ok bool
		out, ok, err = r.CAS(ctx, s.ID, session.StatusStarting, &fence, session.Patch{To: session.StatusActive, At: now, StartedAt: &now, ExpiresAt: &exp})
		require.True(t, ok)
		return err
	})
	return out, out.RuntimeFence
}

func sessionInsertRoundTrip(t *testing.T, h SessionHarness) {
	s := h.Build(t, nil)
	within(t, h, func(ctx context.Context, tx session.Tx) error { return tx.Sessions().Insert(ctx, s) })
	within(t, h, func(ctx context.Context, tx session.Tx) error {
		got, err := tx.Sessions().GetByID(ctx, s.ID)
		require.NoError(t, err)
		require.Equal(t, session.StatusReserved, got.Status)
		require.Equal(t, s.UserID, got.UserID)
		require.Equal(t, s.DurationSeconds, got.DurationSeconds)
		require.Equal(t, int64(0), got.RuntimeFence)
		require.Equal(t, int64(1), got.Version)
		require.JSONEq(t, string(s.ConfigSnapshot), string(got.ConfigSnapshot), "the snapshot round-trips whole")

		_, err = tx.Sessions().GetByID(ctx, uuid.New())
		require.ErrorIs(t, err, session.ErrNotFound)
		owner, err := tx.Sessions().OwnerOf(ctx, s.ID)
		require.NoError(t, err)
		require.Equal(t, s.UserID, owner)

		byKey, err := tx.Sessions().FindByIdempotencyKey(ctx, s.UserID, s.CreateIdempotencyKey)
		require.NoError(t, err)
		require.Equal(t, s.ID, byKey.ID)
		open, err := tx.Sessions().FindOpenByUser(ctx, s.UserID)
		require.NoError(t, err)
		require.Equal(t, s.ID, open.ID)
		rec, err := tx.Sessions().GetRuntimeRecord(ctx, s.ID)
		require.NoError(t, err)
		require.Equal(t, s.ID, rec.ID)
		require.JSONEq(t, string(s.ConfigSnapshot), string(rec.ConfigSnapshot))

		_, err = tx.Sessions().FindOpenByUser(ctx, uuid.New())
		require.ErrorIs(t, err, session.ErrNotFound)
		return nil
	})
}

func sessionUniqueness(t *testing.T, h SessionHarness) {
	first := h.Build(t, nil)
	within(t, h, func(ctx context.Context, tx session.Tx) error { return tx.Sessions().Insert(ctx, first) })

	second := h.Build(t, &first.UserID)
	err := h.Store.WithinTx(context.Background(), func(ctx context.Context, tx session.Tx) error { return tx.Sessions().Insert(ctx, second) })
	require.ErrorIs(t, err, session.ErrAlreadyOpen)

	same := h.Build(t, &first.UserID)
	same.CreateIdempotencyKey = first.CreateIdempotencyKey
	err = h.Store.WithinTx(context.Background(), func(ctx context.Context, tx session.Tx) error { return tx.Sessions().Insert(ctx, same) })
	require.True(t, errors.Is(err, session.ErrDuplicateIdempotency) || errors.Is(err, session.ErrAlreadyOpen), "either unique index may fire first: %v", err)
}

func sessionCAS(t *testing.T, h SessionHarness) {
	s := h.Build(t, nil)
	now := sessionNow()
	within(t, h, func(ctx context.Context, tx session.Tx) error { return tx.Sessions().Insert(ctx, s) })
	within(t, h, func(ctx context.Context, tx session.Tx) error {
		r := tx.Sessions()
		wrongFrom, ok, err := r.CAS(ctx, s.ID, session.StatusActive, nil, session.Patch{To: session.StatusEnding, At: now})
		require.NoError(t, err)
		require.False(t, ok, "status does not match")
		require.Equal(t, session.Session{}.ID, wrongFrom.ID)

		bad := int64(7)
		_, ok, err = r.CAS(ctx, s.ID, session.StatusReserved, &bad, session.Patch{To: session.StatusStarting, At: now, StartingAt: &now})
		require.NoError(t, err)
		require.False(t, ok, "fence does not match")

		got, ok, err := r.CAS(ctx, s.ID, session.StatusReserved, nil, session.Patch{To: session.StatusStarting, At: now, StartingAt: &now})
		require.NoError(t, err)
		require.True(t, ok)
		require.Equal(t, session.StatusStarting, got.Status)
		require.Equal(t, int64(2), got.Version)
		require.Equal(t, int64(0), got.RuntimeFence)
		require.NotNil(t, got.StartingAt)

		bumped, ok, err := r.CAS(ctx, s.ID, session.StatusStarting, nil, session.Patch{To: session.StatusFailed, At: now, BumpFence: true,
			EndReason: ptr(session.EndServerError), FailureDetail: ptr("starting_stuck"), EndedAt: &now})
		require.NoError(t, err)
		require.True(t, ok)
		require.Equal(t, session.StatusFailed, bumped.Status)
		require.Equal(t, int64(1), bumped.RuntimeFence, "a non-runtime terminal transition raises the fence")
		require.Equal(t, int64(3), bumped.Version)
		require.NotNil(t, bumped.EndReason)
		require.Equal(t, session.EndServerError, *bumped.EndReason)
		return nil
	})
}

func ptr[T any](v T) *T { return &v }

func sessionClaim(t *testing.T, h SessionHarness) {
	s, fence := insertActive(t, h)
	require.Equal(t, int64(1), fence)
	now := sessionNow()
	within(t, h, func(ctx context.Context, tx session.Tx) error {
		r := tx.Sessions()
		ok, err := r.MarkHandoff(ctx, s.ID, fence, now)
		require.NoError(t, err)
		require.True(t, ok)
		before, err := r.GetByID(ctx, s.ID)
		require.NoError(t, err)
		require.NotNil(t, before.HandoffAt)

		claimed, err := r.Claim(ctx, s.ID, []session.Status{session.StatusActive})
		require.NoError(t, err)
		require.Equal(t, int64(2), claimed.RuntimeFence)
		require.Equal(t, before.Version, claimed.Version, "Claim does not bump version")
		require.Nil(t, claimed.HandoffAt, "attaching ends the handoff")

		_, err = r.Claim(ctx, s.ID, []session.Status{session.StatusReserved})
		require.ErrorIs(t, err, session.ErrInvalidTransition)
		return nil
	})
}

func sessionAddDisconnected(t *testing.T, h SessionHarness) {
	s, fence := insertActive(t, h)
	now := *s.StartedAt
	within(t, h, func(ctx context.Context, tx session.Tx) error {
		r := tx.Sessions()
		got, ok, err := r.AddDisconnected(ctx, s.ID, fence, 12, now.Add(20*time.Second))
		require.NoError(t, err)
		require.True(t, ok)
		require.Equal(t, 12, got.DisconnectedSeconds)
		require.Equal(t, s.Version+1, got.Version)

		_, ok, err = r.AddDisconnected(ctx, s.ID, fence+1, 5, now.Add(30*time.Second))
		require.NoError(t, err)
		require.False(t, ok, "wrong fence")

		// an episode that ends 5 s after expires_at counts only up to expires_at
		late := s.ExpiresAt.Add(5 * time.Second)
		got, ok, err = r.AddDisconnected(ctx, s.ID, fence, 20, late)
		require.NoError(t, err)
		require.True(t, ok)
		require.Equal(t, 12+15, got.DisconnectedSeconds)

		// never more than the duration
		got, ok, err = r.AddDisconnected(ctx, s.ID, fence, 100000, now.Add(40*time.Second))
		require.NoError(t, err)
		require.True(t, ok)
		require.Equal(t, got.DurationSeconds, got.DisconnectedSeconds)
		return nil
	})
}

func sessionMarkHandoff(t *testing.T, h SessionHarness) {
	s, fence := insertActive(t, h)
	within(t, h, func(ctx context.Context, tx session.Tx) error {
		r := tx.Sessions()
		ok, err := r.MarkHandoff(ctx, s.ID, fence+1, sessionNow())
		require.NoError(t, err)
		require.False(t, ok)
		ok, err = r.MarkHandoff(ctx, s.ID, fence, sessionNow())
		require.NoError(t, err)
		require.True(t, ok)
		got, err := r.GetByID(ctx, s.ID)
		require.NoError(t, err)
		require.Equal(t, s.Version, got.Version, "handoff_at moves, version does not")
		return nil
	})
}

func turnStart(s session.Session, seq int) session.TurnStart {
	return session.TurnStart{ID: uuid.Must(uuid.NewV7()), SessionID: s.ID, Sequence: seq, Generation: 1, CreatedAt: sessionNow()}
}

func sessionTurnsFenced(t *testing.T, h SessionHarness) {
	s, fence := insertActive(t, h)
	ctx := context.Background()
	t1 := turnStart(s, 1)
	within(t, h, func(ctx context.Context, tx session.Tx) error {
		require.NoError(t, tx.Turns().Insert(ctx, fence, t1))
		text := "こんにちは"
		prov := "FAKE_PRIMARY"
		ms := 120
		require.NoError(t, tx.Turns().UpdateUser(ctx, fence, t1.ID, session.TurnUser{CreatedAt: t1.CreatedAt, Text: text, STTProvider: &prov, STTFinalMS: &ms}))
		voice := "voice-actual"
		reply := "はじめまして"
		require.NoError(t, tx.Turns().Finalize(ctx, fence, t1.ID, session.TurnFinal{CreatedAt: t1.CreatedAt, Status: session.TurnCompleted, AssistantText: &reply,
			AssistantSpokenText: &reply, TTSVoiceID: &voice, UsedFallback: true, CompletedAt: sessionNow(), Generation: ptr(5)}))
		got, err := tx.Turns().ListRecent(ctx, s.ID, 10)
		require.NoError(t, err)
		require.Len(t, got, 1)
		require.NotNil(t, got[0].Status)
		require.Equal(t, session.TurnCompleted, *got[0].Status)
		require.Equal(t, "voice-actual", *got[0].TTSVoiceID, "tts_voice_id is stored by Finalize")
		require.True(t, got[0].UsedFallback)
		require.Equal(t, "こんにちは", *got[0].UserText)
		require.Equal(t, 5, got[0].Generation, "Finalize stores the highest generation issued, which a takeover continues above (§7.9.4)")
		_, maxGen, err := tx.Turns().MaxSeqAndGeneration(ctx, s.ID)
		require.NoError(t, err)
		require.Equal(t, 5, maxGen)
		return nil
	})

	// a second claim moves the fence: the old runtime can no longer write
	within(t, h, func(ctx context.Context, tx session.Tx) error {
		_, err := tx.Sessions().Claim(ctx, s.ID, []session.Status{session.StatusActive})
		return err
	})
	err := h.Store.WithinTx(ctx, func(ctx context.Context, tx session.Tx) error { return tx.Turns().Insert(ctx, fence, turnStart(s, 2)) })
	require.ErrorIs(t, err, session.ErrFenceLost)
	err = h.Store.WithinTx(ctx, func(ctx context.Context, tx session.Tx) error {
		return tx.Turns().Finalize(ctx, fence, t1.ID, session.TurnFinal{CreatedAt: t1.CreatedAt, Status: session.TurnFailed, CompletedAt: sessionNow()})
	})
	require.ErrorIs(t, err, session.ErrFenceLost)
	within(t, h, func(ctx context.Context, tx session.Tx) error {
		return tx.Turns().Insert(ctx, fence+1, turnStart(s, 2))
	})

	// a terminal session accepts no runtime write at all
	now := sessionNow()
	within(t, h, func(ctx context.Context, tx session.Tx) error {
		_, ok, err := tx.Sessions().CAS(ctx, s.ID, session.StatusActive, nil, session.Patch{To: session.StatusEnding, At: now, EndingAt: &now, EndReason: ptr(session.EndUserEnded), BumpFence: true})
		require.True(t, ok)
		return err
	})
	err = h.Store.WithinTx(ctx, func(ctx context.Context, tx session.Tx) error {
		return tx.Turns().Insert(ctx, fence+1, turnStart(s, 3))
	})
	require.ErrorIs(t, err, session.ErrFenceLost, "the bumped fence of a non-runtime ENDING transition stops the runtime")
}

func sessionTurnsFailInProgress(t *testing.T, h SessionHarness) {
	s, fence := insertActive(t, h)
	ctx := context.Background()
	a, b := turnStart(s, 1), turnStart(s, 2)
	within(t, h, func(ctx context.Context, tx session.Tx) error {
		require.NoError(t, tx.Turns().Insert(ctx, fence, a))
		reply := "done"
		require.NoError(t, tx.Turns().Finalize(ctx, fence, a.ID, session.TurnFinal{CreatedAt: a.CreatedAt, Status: session.TurnCompleted, AssistantText: &reply, CompletedAt: sessionNow()}))
		require.NoError(t, tx.Turns().Insert(ctx, fence, b))
		text := "sedang berjalan"
		require.NoError(t, tx.Turns().UpdateUser(ctx, fence, b.ID, session.TurnUser{CreatedAt: b.CreatedAt, Text: text}))
		maxSeq, maxGen, err := tx.Turns().MaxSeqAndGeneration(ctx, s.ID)
		require.NoError(t, err)
		require.Equal(t, 2, maxSeq)
		require.Equal(t, 1, maxGen)
		return nil
	})
	within(t, h, func(ctx context.Context, tx session.Tx) error {
		n, err := tx.Turns().FailInProgress(ctx, fence, s.ID, nil)
		require.NoError(t, err)
		require.Equal(t, 1, n)
		n, err = tx.Turns().FailInProgress(ctx, fence, s.ID, nil)
		require.NoError(t, err)
		require.Zero(t, n, "idempotent")
		got, err := tx.Turns().ListRecent(ctx, s.ID, 10)
		require.NoError(t, err)
		require.Len(t, got, 2)
		require.Equal(t, session.TurnFailed, *got[1].Status)
		require.Equal(t, "TAKEOVER", *got[1].ErrorCode)
		require.Equal(t, 1, got[0].Generation, "a Finalize without Generation leaves the column as the INSERT wrote it")
		turns, users, _, err := tx.Turns().Stats(ctx, s.ID, *s.StartingAt)
		require.NoError(t, err)
		require.Equal(t, 2, turns)
		require.Equal(t, 1, users)
		list, err := tx.Turns().ListForUser(ctx, s.ID, *s.StartingAt, nil, nil, 10)
		require.NoError(t, err)
		require.Len(t, list, 2)
		after := 1
		list, err = tx.Turns().ListForUser(ctx, s.ID, *s.StartingAt, nil, &after, 10)
		require.NoError(t, err)
		require.Len(t, list, 1)
		return nil
	})
	// the fence is checked even when nothing is in progress
	err := h.Store.WithinTx(ctx, func(ctx context.Context, tx session.Tx) error {
		_, err := tx.Turns().FailInProgress(ctx, fence+9, s.ID, nil)
		return err
	})
	require.ErrorIs(t, err, session.ErrFenceLost)
	within(t, h, func(ctx context.Context, tx session.Tx) error {
		n, err := tx.Turns().FailDangling(ctx, s.ID, *s.StartingAt, sessionNow())
		require.NoError(t, err)
		require.Zero(t, n)
		maxSeq, _, err := tx.Turns().MaxSeqAndGeneration(ctx, uuid.New())
		require.NoError(t, err)
		require.Equal(t, -1, maxSeq, "no turn yet")
		return nil
	})
}

func sessionEventsFenced(t *testing.T, h SessionHarness) {
	s, fence := insertActive(t, h)
	ctx := context.Background()
	ev := func() session.Event {
		return session.Event{ID: uuid.Must(uuid.NewV7()), SessionID: s.ID, Type: session.EventDisconnect, Actor: session.ActorRealtime,
			Payload: json.RawMessage(`{"at":"2026-10-14T09:00:00Z"}`), CreatedAt: sessionNow()}
	}
	within(t, h, func(ctx context.Context, tx session.Tx) error { return tx.Events().AppendFenced(ctx, fence, ev()) })
	err := h.Store.WithinTx(ctx, func(ctx context.Context, tx session.Tx) error { return tx.Events().AppendFenced(ctx, fence+1, ev()) })
	require.ErrorIs(t, err, session.ErrFenceLost)

	// a lifecycle event needs no fence
	from, to := session.StatusActive, session.StatusEnding
	within(t, h, func(ctx context.Context, tx session.Tx) error {
		return tx.Events().Append(ctx, session.Event{ID: uuid.Must(uuid.NewV7()), SessionID: s.ID, Type: session.EventEnding, Actor: session.ActorSweeper,
			FromStatus: &from, ToStatus: &to, CreatedAt: sessionNow()})
	})
}

func sessionSnapshotImmutable(t *testing.T, h SessionHarness) {
	s, fence := insertActive(t, h)
	want := string(s.ConfigSnapshot)
	now := sessionNow()
	within(t, h, func(ctx context.Context, tx session.Tx) error {
		r := tx.Sessions()
		_, _, err := r.AddDisconnected(ctx, s.ID, fence, 3, now)
		require.NoError(t, err)
		_, err = r.MarkHandoff(ctx, s.ID, fence, now)
		require.NoError(t, err)
		_, err = r.Claim(ctx, s.ID, []session.Status{session.StatusActive})
		require.NoError(t, err)
		_, _, err = r.CAS(ctx, s.ID, session.StatusActive, nil, session.Patch{To: session.StatusEnding, At: now, EndingAt: &now, EndReason: ptr(session.EndUserEnded), BumpFence: true})
		require.NoError(t, err)
		got, err := r.GetByID(ctx, s.ID)
		require.NoError(t, err)
		require.JSONEq(t, want, string(got.ConfigSnapshot))
		return nil
	})
}
