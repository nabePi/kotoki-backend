package session_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/platform/errs"
	"github.com/nabePi/kotoki-backend/internal/session"
	"github.com/nabePi/kotoki-backend/internal/testutil/fakes"
)

func (e *env) query() *session.QueryService {
	return session.NewQueryService(fakes.NewSessionViews(e.store, e.repo), e.store.TurnReader(), e.clk, 180)
}

func TestGetOtherUser404SessionNotFound(t *testing.T) {
	e := newEnv(t, 1800)
	s := e.reserved(t, "key-0000001")
	_, err := e.query().Get(context.Background(), uuid.New(), s.ID)
	require.Equal(t, errs.CodeSessionNotFound, errs.CodeOf(err))
	_, err = e.query().Get(context.Background(), e.userID, uuid.New())
	require.Equal(t, errs.CodeSessionNotFound, errs.CodeOf(err))
	got, err := e.query().Get(context.Background(), e.userID, s.ID)
	require.NoError(t, err)
	require.Equal(t, s.ID, got.ID)
}

func TestGetNameSources(t *testing.T) {
	e := newEnv(t, 1800)
	avatar := "haruka"
	e.character.AvatarKey = &avatar
	e.putContent()
	s := e.reserved(t, "key-0000001")

	// the live character is edited and then disabled; the session keeps what it was created with
	e.character.Name = "Haruka (new)"
	e.character.AvatarKey = nil
	e.character.Status = "DISABLED"
	e.putContent()

	got, err := e.query().Get(context.Background(), e.userID, s.ID)
	require.NoError(t, err)
	require.Equal(t, "Haruka", got.CharacterName, "from config_snapshot.character")
	require.NotNil(t, got.AvatarKey)
	require.Equal(t, "haruka", *got.AvatarKey)
	require.Equal(t, e.topic.Name, got.TopicName)
	require.Equal(t, e.scenario.Name, got.ScenarioName)
	require.Equal(t, t0.Add(5*time.Minute), got.StartDeadlineAt)
}

func TestListStatusFilterAndKeyset(t *testing.T) {
	e := newEnv(t, 3*900)
	ctx := context.Background()
	var ids []uuid.UUID
	for i, key := range []string{"key-0000001", "key-0000002", "key-0000003"} {
		s := e.reserved(t, key)
		ids = append(ids, s.ID)
		if i < 2 {
			_, err := e.svc.Cancel(ctx, e.userID, s.ID)
			require.NoError(t, err)
		}
	}
	q := e.query()
	all, err := q.List(ctx, e.userID, nil, nil, 10)
	require.NoError(t, err)
	require.Len(t, all, 3)
	for i := 1; i < len(all); i++ {
		require.Greater(t, all[i-1].ID.String(), all[i].ID.String(), "id DESC")
	}

	cancelled, err := q.List(ctx, e.userID, []session.Status{session.StatusCancelled}, nil, 10)
	require.NoError(t, err)
	require.Len(t, cancelled, 2)
	reserved, err := q.List(ctx, e.userID, []session.Status{session.StatusReserved}, nil, 10)
	require.NoError(t, err)
	require.Len(t, reserved, 1)
	require.Equal(t, ids[2], reserved[0].ID)

	page, err := q.List(ctx, e.userID, nil, nil, 2+1) // limit+1 tells the handler there is a next page
	require.NoError(t, err)
	require.Len(t, page, 3)
	next, err := q.List(ctx, e.userID, nil, &page[1].ID, 3)
	require.NoError(t, err)
	require.Len(t, next, 1)
	require.Equal(t, all[2].ID, next[0].ID)

	other, err := q.List(ctx, uuid.New(), nil, nil, 10)
	require.NoError(t, err)
	require.Empty(t, other, "another user sees nothing")
}

func (e *env) turn(t *testing.T, fence int64, s session.Session, seq int, user string, st *session.TurnStatus) {
	t.Helper()
	id := uuid.Must(uuid.NewV7())
	ctx := context.Background()
	require.NoError(t, e.store.WithinTx(ctx, func(ctx context.Context, tx session.Tx) error {
		require.NoError(t, tx.Turns().Insert(ctx, fence, session.TurnStart{ID: id, SessionID: s.ID, Sequence: seq, Generation: 1, CreatedAt: tenAM}))
		if user != "" {
			require.NoError(t, tx.Turns().UpdateUser(ctx, fence, id, session.TurnUser{CreatedAt: tenAM, Text: user}))
		}
		if st == nil {
			return nil
		}
		reply := "r"
		return tx.Turns().Finalize(ctx, fence, id, session.TurnFinal{CreatedAt: tenAM, Status: *st, AssistantText: &reply, CompletedAt: tenAM})
	}))
}

func TestTurnsOnlyCompletedInterruptedFailed(t *testing.T) {
	e := newEnv(t, 1800)
	s, fence := e.active(t, 600)
	ptr := func(st session.TurnStatus) *session.TurnStatus { return &st }
	e.turn(t, fence, s, 1, "a", ptr(session.TurnCompleted))
	e.turn(t, fence, s, 2, "b", ptr(session.TurnInterrupted))
	e.turn(t, fence, s, 3, "c", ptr(session.TurnFailed))
	e.turn(t, fence, s, 4, "d", ptr(session.TurnDiscarded))
	e.turn(t, fence, s, 5, "e", nil) // still running

	got, err := e.query().Turns(context.Background(), e.userID, s.ID, nil, 50)
	require.NoError(t, err, "an ACTIVE session may be read")
	require.Len(t, got, 3)
	for i, want := range []int{1, 2, 3} {
		require.Equal(t, want, got[i].Sequence)
	}
	after := 1
	got, err = e.query().Turns(context.Background(), e.userID, s.ID, &after, 1)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, 2, got[0].Sequence)

	_, err = e.query().Turns(context.Background(), uuid.New(), s.ID, nil, 50)
	require.Equal(t, errs.CodeSessionNotFound, errs.CodeOf(err))
}

func TestTurnsActiveSessionAllowed(t *testing.T) {
	e := newEnv(t, 1800)
	s, _ := e.active(t, 600)
	got, err := e.query().Turns(context.Background(), e.userID, s.ID, nil, 50)
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Empty(t, got)
}

func TestTurns410WhenPurgedAt(t *testing.T) {
	e := newEnv(t, 1800)
	s, fence := e.active(t, 600)
	e.clk.Set(at(10, 7, 0, 0))
	_, err := e.svc.BeginEnding(context.Background(), s.ID, &fence, session.EndUserEnded, "", session.ActorRealtime)
	require.NoError(t, err)
	_, err = e.svc.Finalize(context.Background(), s.ID, &fence, session.ActorRealtime)
	require.NoError(t, err)
	purged := e.store.Session(s.ID)
	purgedAt := e.clk.Now()
	purged.TranscriptPurgedAt = &purgedAt
	e.store.PutSession(purged)

	_, err = e.query().Turns(context.Background(), e.userID, s.ID, nil, 50)
	require.Equal(t, errs.CodeTranscriptExpired, errs.CodeOf(err))
}

func TestTurns410WhenEndedBeforeRetention(t *testing.T) {
	e := newEnv(t, 1800)
	s, fence := e.active(t, 600)
	e.clk.Set(at(10, 7, 0, 0))
	_, err := e.svc.BeginEnding(context.Background(), s.ID, &fence, session.EndUserEnded, "", session.ActorRealtime)
	require.NoError(t, err)
	_, err = e.svc.Finalize(context.Background(), s.ID, &fence, session.ActorRealtime)
	require.NoError(t, err)
	ended := *e.store.Session(s.ID).EndedAt

	e.clk.Set(ended.Add(179 * 24 * time.Hour))
	_, err = e.query().Turns(context.Background(), e.userID, s.ID, nil, 50)
	require.NoError(t, err, "179 days: still there")
	e.clk.Set(ended.Add(181 * 24 * time.Hour))
	_, err = e.query().Turns(context.Background(), e.userID, s.ID, nil, 50)
	require.Equal(t, errs.CodeTranscriptExpired, errs.CodeOf(err), "181 days: gone")
}

func TestTurnsReservedSessionEmpty(t *testing.T) {
	e := newEnv(t, 1800)
	s := e.reserved(t, "key-0000001")
	got, err := e.query().Turns(context.Background(), e.userID, s.ID, nil, 50)
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Empty(t, got)
	require.Zero(t, e.store.ListForUserCalls, "no starting_at: no query at all")
}

func TestTurnsPruningBounds(t *testing.T) {
	e := newEnv(t, 1800)
	s, fence := e.active(t, 600)
	_, err := e.query().Turns(context.Background(), e.userID, s.ID, nil, 50)
	require.NoError(t, err)
	require.Equal(t, 1, e.store.ListForUserCalls)
	require.Equal(t, tenAM, e.store.LastListForUser.StartingAt, "the repository widens it by one minute")
	require.Nil(t, e.store.LastListForUser.EndedAt, "an open session has no upper bound")

	e.clk.Set(at(10, 7, 0, 0))
	_, err = e.svc.BeginEnding(context.Background(), s.ID, &fence, session.EndUserEnded, "", session.ActorRealtime)
	require.NoError(t, err)
	done, err := e.svc.Finalize(context.Background(), s.ID, &fence, session.ActorRealtime)
	require.NoError(t, err)
	_, err = e.query().Turns(context.Background(), e.userID, s.ID, nil, 50)
	require.NoError(t, err)
	require.Equal(t, 2, e.store.ListForUserCalls)
	require.NotNil(t, e.store.LastListForUser.EndedAt)
	require.Equal(t, *done.EndedAt, *e.store.LastListForUser.EndedAt)
}
