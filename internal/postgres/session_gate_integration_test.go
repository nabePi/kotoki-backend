//go:build integration

package postgres_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/admin"
	"github.com/nabePi/kotoki-backend/internal/auth"
	"github.com/nabePi/kotoki-backend/internal/balance"
	"github.com/nabePi/kotoki-backend/internal/content"
	"github.com/nabePi/kotoki-backend/internal/platform/clock"
	"github.com/nabePi/kotoki-backend/internal/postgres"
	"github.com/nabePi/kotoki-backend/internal/session"
	"github.com/nabePi/kotoki-backend/internal/testutil"
	"github.com/nabePi/kotoki-backend/internal/testutil/fakes"
)

// wireSessions builds the session service over p's store and injects it into the admin gate.
func (p pgAdmin) wireSessions(t *testing.T) (*session.Service, testutil.ContentIDs) {
	t.Helper()
	ids := testutil.SeedContent(t, p.pool)
	clk := clock.NewFake(t0)
	svc := session.NewService(session.Deps{
		Store: p.store.SessionStore(), Reader: postgres.NewSessionRepo(p.pool), Balance: balance.NewService(p.store.BalanceStore(), clk, p.ids),
		Resolver: content.NewResolver(postgres.NewContentRepo(p.pool), content.ResolverConfig{TTSMode: "fake", LLMConfigured: true}, nil),
		Adaptive: fakes.NewAdaptive(""), Users: fakes.NewUserGate(session.UserGateInfo{Status: "ACTIVE", TermsVersion: "v1"}),
		OpeningPrompts: &fakes.OpeningSource{}, Clock: clk, IDs: p.ids, TermsVersion: "v1", ServiceName: "api",
	})
	p.store.SetSessionCanceller(svc)
	return svc, ids
}

func (p pgAdmin) reserve(t *testing.T, svc *session.Service, userID uuid.UUID, c testutil.ContentIDs, key string) session.Session {
	t.Helper()
	res, err := svc.Create(context.Background(), session.CreateSessionInput{UserID: userID, IdempotencyKey: key, TopicID: c.TopicID, ScenarioID: c.ScenarioID,
		CharacterID: c.CharacterID, DurationSeconds: 300, ConversationStartMode: session.StartUserFirst})
	require.NoError(t, err)
	return res.Session
}

func TestPGSessionGateCancelReserved(t *testing.T) {
	p := newPGAdmin(t)
	svc, c := p.wireSessions(t)
	ctx := context.Background()
	u := p.user(t, auth.RoleUser, 1800)
	s := p.reserve(t, svc, u, c, "gate-key-0001")

	require.NoError(t, p.store.AdminStore().WithinTx(ctx, func(ctx context.Context, tx admin.Tx) error {
		return tx.SessionGate().CancelReserved(ctx, s.ID, t0)
	}))
	got, err := postgres.NewSessionRepo(p.pool).GetByID(ctx, s.ID)
	require.NoError(t, err)
	require.Equal(t, session.StatusCancelled, got.Status)
	require.Equal(t, session.EndUserCancelled, *got.EndReason)

	var resStatus, actor string
	require.NoError(t, p.pool.QueryRow(ctx, `SELECT status FROM balance_reservations WHERE session_id = $1`, s.ID).Scan(&resStatus))
	require.Equal(t, "RELEASED", resStatus)
	require.NoError(t, p.pool.QueryRow(ctx, `SELECT actor FROM session_events WHERE session_id = $1 AND event_type = 'SESSION_CANCELLED'`, s.ID).Scan(&actor))
	require.Equal(t, "ADMIN", actor)
	var releases int
	require.NoError(t, p.pool.QueryRow(ctx, `SELECT count(*) FROM balance_ledger WHERE entry_type = 'RELEASE' AND session_id = $1`, s.ID).Scan(&releases))
	require.Equal(t, 1, releases)
	testutil.AssertReconciled(t, p.pool)
}

func TestPGSessionGateCancelReservedRefusesNonReserved(t *testing.T) {
	p := newPGAdmin(t)
	svc, c := p.wireSessions(t)
	ctx := context.Background()
	u := p.user(t, auth.RoleUser, 1800)
	s := p.reserve(t, svc, u, c, "gate-key-0002")
	var fence int64
	require.NoError(t, p.store.SessionStore().WithinTx(ctx, func(ctx context.Context, tx session.Tx) error {
		claimed, err := tx.Sessions().Claim(ctx, s.ID, []session.Status{session.StatusReserved})
		fence = claimed.RuntimeFence
		return err
	}))
	_, err := svc.BeginStart(ctx, s.ID, fence)
	require.NoError(t, err)

	err = p.store.AdminStore().WithinTx(ctx, func(ctx context.Context, tx admin.Tx) error {
		return tx.SessionGate().CancelReserved(ctx, s.ID, t0)
	})
	require.Error(t, err)
	require.True(t, errors.Is(err, session.ErrInvalidTransition))
	got, err := postgres.NewSessionRepo(p.pool).GetByID(ctx, s.ID)
	require.NoError(t, err)
	require.Equal(t, session.StatusStarting, got.Status)

	// not wired at all: a clear internal error, never a silent skip
	bare := newPGAdmin(t)
	err = bare.store.AdminStore().WithinTx(ctx, func(ctx context.Context, tx admin.Tx) error {
		return tx.SessionGate().CancelReserved(ctx, uuid.New(), t0)
	})
	require.Error(t, err)
}

// TestPGDeleteAccountCancelsReservedSession is the M2 stub case turned around: deleting an account
// that still has a RESERVED session cancels it and returns the minutes.
func TestPGDeleteAccountCancelsReservedSession(t *testing.T) {
	p := newPGAdmin(t)
	svc, c := p.wireSessions(t)
	ctx := context.Background()
	u := p.user(t, auth.RoleUser, 1800)
	s := p.reserve(t, svc, u, c, "gate-key-0003")

	require.NoError(t, p.svc.DeleteAccount(ctx, u, t0))

	var userStatus, sessStatus, resStatus string
	require.NoError(t, p.pool.QueryRow(ctx, `SELECT status FROM users WHERE id = $1`, u).Scan(&userStatus))
	require.NoError(t, p.pool.QueryRow(ctx, `SELECT status FROM sessions WHERE id = $1`, s.ID).Scan(&sessStatus))
	require.NoError(t, p.pool.QueryRow(ctx, `SELECT status FROM balance_reservations WHERE session_id = $1`, s.ID).Scan(&resStatus))
	require.Equal(t, "DELETED", userStatus)
	require.Equal(t, "CANCELLED", sessStatus)
	require.Equal(t, "RELEASED", resStatus)
	var available, reserved int64
	require.NoError(t, p.pool.QueryRow(ctx, `SELECT available_seconds, reserved_seconds FROM balance_accounts WHERE user_id = $1`, u).Scan(&available, &reserved))
	require.Equal(t, int64(1800), available)
	require.Zero(t, reserved)
	testutil.AssertReconciled(t, p.pool)
}
