package session_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/content"
	"github.com/nabePi/kotoki-backend/internal/session"
)

// conversationRuntime is a copy of conversation.SessionRuntime of spec 7.13.1; M5 declares the real
// one on the consumer side and *session.RuntimeService has to satisfy it.
type conversationRuntime interface {
	GetForAttach(ctx context.Context, sessionID uuid.UUID) (session.RuntimeRecord, error)
	ClaimRuntime(ctx context.Context, sessionID uuid.UUID, allowed []session.Status) (fence int64, rec session.RuntimeRecord, err error)
	MarkStarting(ctx context.Context, sessionID uuid.UUID, fence int64) (session.RuntimeRecord, error)
	Activate(ctx context.Context, sessionID uuid.UUID, fence int64) (startedAt, expiresAt time.Time, err error)
	FailStart(ctx context.Context, sessionID uuid.UUID, fence int64, reason session.EndReason) (session.Settlement, error)
	AddDisconnected(ctx context.Context, sessionID uuid.UUID, fence int64, seconds int) error
	MarkHandoff(ctx context.Context, sessionID uuid.UUID, fence int64) error
	BeginEnding(ctx context.Context, sessionID uuid.UUID, fence int64, reason session.EndReason) error
	Complete(ctx context.Context, sessionID uuid.UUID, fence int64) (session.Settlement, error)
	Fail(ctx context.Context, sessionID uuid.UUID, fence int64, reason session.EndReason) (session.Settlement, error)
}

var _ conversationRuntime = (*session.RuntimeService)(nil)

func TestRuntimeServiceSatisfiesConversationPort(t *testing.T) {
	var rt conversationRuntime = session.NewRuntimeService(newEnv(t, 0).svc, nil)
	require.NotNil(t, rt)
}

func (e *env) runtime() *session.RuntimeService {
	return session.NewRuntimeService(e.svc, e.store.Reader())
}

func TestMarkStartingReturnsOpeningPromptID(t *testing.T) {
	e := newEnv(t, 1800)
	opening := uuid.New()
	e.opening.Set(&opening)
	rt := e.runtime()
	s := e.reserved(t, "key-0000001", func(in *session.CreateSessionInput) { in.ConversationStartMode = session.StartAIFirst })
	fence, rec, err := rt.ClaimRuntime(context.Background(), s.ID, []session.Status{session.StatusReserved})
	require.NoError(t, err)
	require.Equal(t, int64(1), fence)
	require.Equal(t, session.StatusReserved, rec.Status)

	rec, err = rt.MarkStarting(context.Background(), s.ID, fence)
	require.NoError(t, err)
	require.Equal(t, session.StatusStarting, rec.Status)
	require.NotNil(t, rec.OpeningPromptVersionID)
	require.Equal(t, opening, *rec.OpeningPromptVersionID)
	require.Equal(t, fence, rec.RuntimeFence)
	require.Equal(t, e.character.ID, rec.CharacterID)
}

func TestGetForAttachReturnsConfigSnapshot(t *testing.T) {
	e := newEnv(t, 1800)
	s := e.reserved(t, "key-0000001")
	rec, err := e.runtime().GetForAttach(context.Background(), s.ID)
	require.NoError(t, err)
	require.JSONEq(t, string(e.store.Session(s.ID).ConfigSnapshot), string(rec.ConfigSnapshot))
	snap, err := content.ParseSnapshot(rec.ConfigSnapshot)
	require.NoError(t, err)
	require.Equal(t, "HARUKA", snap.Character.Code)
	require.Equal(t, content.FormalityKeigo, snap.Formality)
	require.Equal(t, "ELEVENLABS", snap.Character.ProviderName)
	require.NotNil(t, snap.Character.Voice.Fallback)
	require.JSONEq(t, string(s.AdaptiveDifficulty), string(rec.AdaptiveDifficulty))
	require.Equal(t, e.topic.ID, rec.TopicID)
	require.Equal(t, e.sv.ID, rec.ScenarioVersionID)
	require.Equal(t, e.prompt.ID, rec.PromptVersionID)
	require.Equal(t, e.userID, rec.UserID)
	require.Equal(t, session.StartUserFirst, rec.ConversationStartMode)

	_, err = e.runtime().GetForAttach(context.Background(), uuid.New())
	require.ErrorIs(t, err, session.ErrNotFound)

	// the record keeps no legacy profile field
	raw, _ := json.Marshal(rec)
	for _, banned := range []string{"ProviderProfile", "CharacterVersionID", "VoiceProfileID", "ConversationStyleVersionID", "ExpressionProfileVersionID"} {
		require.NotContains(t, string(raw), banned)
	}
}

func TestActivateThroughFacade(t *testing.T) {
	e := newEnv(t, 1800)
	rt := e.runtime()
	e.clk.Set(tenAM)
	s := e.reserved(t, "key-0000001")
	fence, _, err := rt.ClaimRuntime(context.Background(), s.ID, []session.Status{session.StatusReserved})
	require.NoError(t, err)
	_, err = rt.MarkStarting(context.Background(), s.ID, fence)
	require.NoError(t, err)
	started, expires, err := rt.Activate(context.Background(), s.ID, fence)
	require.NoError(t, err)
	require.Equal(t, tenAM, started)
	require.Equal(t, tenAM.Add(900*time.Second), expires)
}

func TestCompleteReturnsSettlement(t *testing.T) {
	e := newEnv(t, 1800)
	rt := e.runtime()
	s, fence := e.active(t, 600)
	_, err := e.svc.AddDisconnected(context.Background(), s.ID, fence, 10)
	require.NoError(t, err)
	e.clk.Set(at(10, 7, 0, 0))
	require.NoError(t, rt.BeginEnding(context.Background(), s.ID, fence, session.EndUserEnded))
	require.NoError(t, rt.BeginEnding(context.Background(), s.ID, fence, session.EndUserEnded), "no-op when already ENDING")
	e.clk.Set(at(10, 7, 3, 0))
	st, err := rt.Complete(context.Background(), s.ID, fence)
	require.NoError(t, err)

	row := e.store.Session(s.ID)
	require.Equal(t, session.Settlement{ChargedSeconds: row.ChargedSeconds, DisconnectedSeconds: row.DisconnectedSeconds, StartedAt: *row.StartedAt,
		EndedAt: *row.EndedAt, EndReason: session.EndUserEnded}, st)
	require.Equal(t, 410, st.ChargedSeconds, "420 s minus 10 s disconnected")
	require.Equal(t, 10, st.DisconnectedSeconds)

	again, err := rt.Complete(context.Background(), s.ID, fence)
	require.NoError(t, err, "idempotent after the terminal transition")
	require.Equal(t, st, again)
}

func TestFailReasonMismatchUsesStoredReasonAndLogsWarn(t *testing.T) {
	e := newEnv(t, 1800)
	rt := e.runtime()
	s, fence := e.active(t, 600)
	e.clk.Set(at(10, 3, 0, 0))
	require.NoError(t, rt.BeginEnding(context.Background(), s.ID, fence, session.EndProviderFailure))
	st, err := rt.Fail(context.Background(), s.ID, fence, session.EndServerError)
	require.NoError(t, err)
	require.Equal(t, session.EndProviderFailure, st.EndReason, "the first reason wins")
	require.Equal(t, session.StatusFailed, e.store.Session(s.ID).Status)
	require.Contains(t, e.logs.String(), "session.fail_reason_mismatch")
	require.Contains(t, e.logs.String(), `"level":"WARN"`)
}

func TestFailStartThroughFacadeSettlesAtZero(t *testing.T) {
	e := newEnv(t, 1800)
	rt := e.runtime()
	s := e.reserved(t, "key-0000001")
	fence, _, err := rt.ClaimRuntime(context.Background(), s.ID, []session.Status{session.StatusReserved})
	require.NoError(t, err)
	_, err = rt.MarkStarting(context.Background(), s.ID, fence)
	require.NoError(t, err)
	st, err := rt.FailStart(context.Background(), s.ID, fence, session.EndProviderFailure)
	require.NoError(t, err)
	require.Zero(t, st.ChargedSeconds)
	require.Equal(t, session.EndProviderFailure, st.EndReason)
	require.True(t, st.StartedAt.IsZero(), "the session never started")
	require.False(t, st.EndedAt.IsZero())
}

func TestRuntimeCallsWithStaleFenceErrFenceLost(t *testing.T) {
	e := newEnv(t, 3600)
	rt := e.runtime()
	ctx := context.Background()

	s := e.reserved(t, "key-0000001")
	fence, _, err := rt.ClaimRuntime(ctx, s.ID, []session.Status{session.StatusReserved})
	require.NoError(t, err)
	_, err = rt.MarkStarting(ctx, s.ID, fence+1)
	require.ErrorIs(t, err, session.ErrFenceLost, "MarkStarting")
	_, err = rt.MarkStarting(ctx, s.ID, fence)
	require.NoError(t, err)
	_, _, err = rt.Activate(ctx, s.ID, fence+1)
	require.ErrorIs(t, err, session.ErrFenceLost, "Activate")
	_, _, err = rt.Activate(ctx, s.ID, fence)
	require.NoError(t, err)
	require.ErrorIs(t, rt.AddDisconnected(ctx, s.ID, fence+1, 3), session.ErrFenceLost, "AddDisconnected")
	require.ErrorIs(t, rt.MarkHandoff(ctx, s.ID, fence+1), session.ErrFenceLost, "MarkHandoff")
	require.ErrorIs(t, rt.BeginEnding(ctx, s.ID, fence+1, session.EndUserEnded), session.ErrFenceLost, "BeginEnding")
	require.NoError(t, rt.AddDisconnected(ctx, s.ID, fence, 3))
}
