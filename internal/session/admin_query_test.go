package session_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/balance"
	"github.com/nabePi/kotoki-backend/internal/platform/errs"
	"github.com/nabePi/kotoki-backend/internal/session"
	"github.com/nabePi/kotoki-backend/internal/testutil/fakes"
)

func (e *env) adminQuery() *session.AdminQueryService {
	return session.NewAdminQueryService(fakes.NewSessionAdmin(e.store, e.repo), e.clk, 180)
}

const vendorSnapshot = `{"character":{"name":"Haruka","avatar_key":"haruka","persona":"calm","provider_name":"openai","model_name":"gpt-x","voice_id":"v-123",` +
	`"voice":{"gender":"F","accent":"tokyo","speaking_rate":1.1,"fallback":{"voice_id":"v-456"}}},"formality":"polite"}`

func (e *env) withSnapshot(id uuid.UUID, snap string) {
	s := e.store.Session(id)
	s.ConfigSnapshot = json.RawMessage(snap)
	e.store.PutSession(s)
}

func TestAdminGet_IncludesInternalFieldsAndReservation(t *testing.T) {
	e := newEnv(t, 1800)
	s := e.reserved(t, "key-0000001")
	e.withSnapshot(s.ID, vendorSnapshot)

	got, err := e.adminQuery().Get(context.Background(), s.ID)
	require.NoError(t, err)
	require.Equal(t, e.userID, got.UserID)
	require.Equal(t, s.RuntimeFence, got.RuntimeFence)
	require.NotNil(t, got.Reservation)
	require.Equal(t, balance.ReservationHeld, got.Reservation.Status)
	require.Equal(t, s.ID, got.Reservation.SessionID)

	var snap map[string]any
	require.NoError(t, json.Unmarshal(got.ConfigSnapshot, &snap))
	ch := snap["character"].(map[string]any)
	for _, k := range []string{"provider_name", "model_name", "voice_id"} {
		require.Equal(t, "[REDACTED]", ch[k], k)
	}
	voice := ch["voice"].(map[string]any)
	require.Equal(t, "[REDACTED]", voice["fallback"])
	require.Equal(t, "tokyo", voice["accent"])
	require.InDelta(t, 1.1, voice["speaking_rate"], 0)
	require.Equal(t, "calm", ch["persona"], "persona stays")
	require.Equal(t, "polite", snap["formality"])
	require.NotContains(t, snap, "provider_profile")

	// the stored snapshot itself is untouched
	require.JSONEq(t, vendorSnapshot, string(e.store.Session(s.ID).ConfigSnapshot))
}

func TestAdminGet_AnyUser_NotOwnerFiltered(t *testing.T) {
	e := newEnv(t, 1800)
	s := e.reserved(t, "key-0000001")
	got, err := e.adminQuery().Get(context.Background(), s.ID)
	require.NoError(t, err)
	require.Equal(t, s.ID, got.ID)

	_, err = e.adminQuery().Get(context.Background(), uuid.New())
	require.Equal(t, errs.CodeSessionNotFound, errs.CodeOf(err))
}

func TestRedactSnapshot_NonObjectAndMissingKeysUntouched(t *testing.T) {
	require.Equal(t, json.RawMessage(`[1]`), session.RedactSnapshot(json.RawMessage(`[1]`)))
	require.JSONEq(t, `{"character":{"name":"A","voice":{"fallback":null}}}`,
		string(session.RedactSnapshot(json.RawMessage(`{"character":{"name":"A","voice":{"fallback":null}}}`))))
}

func TestAdminTurns_IncludesDiscardedAndInProgress(t *testing.T) {
	e := newEnv(t, 1800)
	s, fence := e.active(t, 600)
	ptr := func(st session.TurnStatus) *session.TurnStatus { return &st }
	e.turn(t, fence, s, 1, "a", ptr(session.TurnCompleted))
	e.turn(t, fence, s, 2, "b", ptr(session.TurnDiscarded))
	e.turn(t, fence, s, 3, "c", nil)

	got, err := e.adminQuery().Turns(context.Background(), s.ID, nil, 50)
	require.NoError(t, err)
	require.Len(t, got, 3)
	require.Nil(t, got[2].Status)

	after := 1
	got, err = e.adminQuery().Turns(context.Background(), s.ID, &after, 1)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, 2, got[0].Sequence)
}

func TestAdminTurns_ReservedSessionEmptyNonNil(t *testing.T) {
	e := newEnv(t, 1800)
	s := e.reserved(t, "key-0000001")
	got, err := e.adminQuery().Turns(context.Background(), s.ID, nil, 50)
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Empty(t, got)
}

func TestAdminTurns_410WhenPurged(t *testing.T) {
	e := newEnv(t, 1800)
	s, fence := e.active(t, 600)
	e.clk.Set(at(10, 7, 0, 0))
	_, err := e.svc.BeginEnding(context.Background(), s.ID, &fence, session.EndUserEnded, "", session.ActorRealtime)
	require.NoError(t, err)
	_, err = e.svc.Finalize(context.Background(), s.ID, &fence, session.ActorRealtime)
	require.NoError(t, err)
	ended := *e.store.Session(s.ID).EndedAt

	e.clk.Set(ended.Add(181 * 24 * time.Hour))
	_, err = e.adminQuery().Turns(context.Background(), s.ID, nil, 50)
	require.Equal(t, errs.CodeTranscriptExpired, errs.CodeOf(err))

	e.clk.Set(ended.Add(time.Hour))
	purged := e.store.Session(s.ID)
	now := e.clk.Now()
	purged.TranscriptPurgedAt = &now
	e.store.PutSession(purged)
	_, err = e.adminQuery().Turns(context.Background(), s.ID, nil, 50)
	require.Equal(t, errs.CodeTranscriptExpired, errs.CodeOf(err))
}

func TestAdminEvents_CursorOrderCreatedAtIdAsc(t *testing.T) {
	e := newEnv(t, 1800)
	s, _ := e.active(t, 600)
	all := e.store.Events(s.ID)
	require.GreaterOrEqual(t, len(all), 3)

	page, err := e.adminQuery().Events(context.Background(), s.ID, nil, 2)
	require.NoError(t, err)
	require.Len(t, page, 2)
	for i := 1; i < len(page); i++ {
		require.False(t, page[i].CreatedAt.Before(page[i-1].CreatedAt))
	}
	last := page[len(page)-1]
	next, err := e.adminQuery().Events(context.Background(), s.ID, &session.EventCursor{CreatedAt: last.CreatedAt, ID: last.ID}, 100)
	require.NoError(t, err)
	require.Len(t, next, len(all)-2, "strictly after the cursor, nothing repeated")
	for _, ev := range next {
		require.NotEqual(t, page[0].ID, ev.ID)
		require.NotEqual(t, last.ID, ev.ID)
	}

	_, err = e.adminQuery().Events(context.Background(), uuid.New(), nil, 10)
	require.Equal(t, errs.CodeSessionNotFound, errs.CodeOf(err))
}

func TestAdminListByUser_StatusFilterKeyset(t *testing.T) {
	e := newEnv(t, 3*900)
	ctx := context.Background()
	for i, key := range []string{"key-0000001", "key-0000002", "key-0000003"} {
		s := e.reserved(t, key)
		if i < 2 {
			_, err := e.svc.Cancel(ctx, e.userID, s.ID)
			require.NoError(t, err)
		}
	}
	q := e.adminQuery()
	all, err := q.ListByUser(ctx, e.userID, nil, nil, 10)
	require.NoError(t, err)
	require.Len(t, all, 3)
	for i := 1; i < len(all); i++ {
		require.Greater(t, all[i-1].ID.String(), all[i].ID.String())
	}
	cancelled, err := q.ListByUser(ctx, e.userID, []session.Status{session.StatusCancelled}, nil, 10)
	require.NoError(t, err)
	require.Len(t, cancelled, 2)
	next, err := q.ListByUser(ctx, e.userID, nil, &all[0].ID, 10)
	require.NoError(t, err)
	require.Len(t, next, 2)
	other, err := q.ListByUser(ctx, uuid.New(), nil, nil, 10)
	require.NoError(t, err)
	require.Empty(t, other)
}
