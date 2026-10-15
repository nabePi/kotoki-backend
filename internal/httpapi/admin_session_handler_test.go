package httpapi_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/admin"
	"github.com/nabePi/kotoki-backend/internal/session"
)

// adminSessionWorld funds a user, creates a RESERVED session and returns an admin Authorization.
func adminSessionWorld(t *testing.T) (e *apiEnv, adm string, owner uuid.UUID, sessionID uuid.UUID) {
	t.Helper()
	e, userAuthz := sessionWorld(t, 1800)
	owner = e.userByEmail(t, "hana@example.com").ID
	var created struct{ ID uuid.UUID }
	require.NoError(t, json.Unmarshal(e.createSession(t, userAuthz, "key-0000001").Body.Bytes(), &created))
	adm, _ = e.adminAuthz(t)
	return e, adm, owner, created.ID
}

// activate moves the session to ACTIVE and returns the runtime fence.
func (e *apiEnv) activate(t *testing.T, id uuid.UUID) int64 {
	t.Helper()
	fence, _, err := e.sessionSvc.ClaimRuntime(t.Context(), id, []session.Status{session.StatusReserved})
	require.NoError(t, err)
	_, err = e.sessionSvc.BeginStart(t.Context(), id, fence)
	require.NoError(t, err)
	_, err = e.sessionSvc.Activate(t.Context(), id, fence)
	require.NoError(t, err)
	return fence
}

func (e *apiEnv) addTurn(t *testing.T, fence int64, sessionID uuid.UUID, seq int, user string, st *session.TurnStatus) {
	t.Helper()
	id := uuid.MustParse("0192f1a0-2222-7000-8000-00000000000" + string(rune('0'+seq)))
	now := e.clk.Now()
	require.NoError(t, e.sessions.WithinTx(t.Context(), func(ctx context.Context, tx session.Tx) error {
		require.NoError(t, tx.Turns().Insert(ctx, fence, session.TurnStart{ID: id, SessionID: sessionID, Sequence: seq, Generation: 1, CreatedAt: now}))
		require.NoError(t, tx.Turns().UpdateUser(ctx, fence, id, session.TurnUser{CreatedAt: now, Text: user}))
		if st == nil {
			return nil
		}
		reply := "reply " + user
		return tx.Turns().Finalize(ctx, fence, id, session.TurnFinal{CreatedAt: now, Status: *st, AssistantText: &reply, CompletedAt: now})
	}))
}

func TestAdminSessionGetGolden(t *testing.T) {
	e, adm, owner, id := adminSessionWorld(t)
	rec := e.do(http.MethodGet, "/api/v1/admin/sessions/"+id.String(), "", "Authorization", adm)
	require.Equal(t, 200, rec.Code, rec.Body.String())
	goldenFile(t, "admin_sessions", "get_200.json", rec.Body.Bytes())

	var m map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &m))
	require.Equal(t, owner.String(), m["user_id"])
	for _, want := range []string{"scenario_version_id", "prompt_version_id", "config_snapshot", "adaptive_difficulty", "runtime_fence", "handoff_at", "failure_detail", "transcript_purged_at", "version", "reservation", "topic", "scenario", "character"} {
		require.Contains(t, m, want)
	}
	res := m["reservation"].(map[string]any)
	require.Equal(t, "HELD", res["status"])
	for _, want := range []string{"id", "reserved_seconds", "consumed_seconds", "released_seconds", "start_deadline_at", "settled_at", "released_at"} {
		require.Contains(t, res, want)
	}
	require.NotContains(t, rec.Body.String(), "21m00Tcm4TlvDq8ikWAM", "vendor voice id is redacted")
	require.Contains(t, rec.Body.String(), "[REDACTED]")
	require.Empty(t, e.adminStore.Audit(), "plain reads write no audit")
}

func TestAdminSessionGetNotFound(t *testing.T) {
	e, adm, _, _ := adminSessionWorld(t)
	for _, id := range []string{uuid.NewString(), "not-a-uuid"} {
		rec := e.do(http.MethodGet, "/api/v1/admin/sessions/"+id, "", "Authorization", adm)
		require.Equal(t, 404, rec.Code, id)
		require.Equal(t, "SESSION_NOT_FOUND", errorBody(t, rec)["code"])
	}
}

func TestAdminUserSessionsListGolden(t *testing.T) {
	e, adm, owner, id := adminSessionWorld(t)
	rec := e.do(http.MethodGet, "/api/v1/admin/users/"+owner.String()+"/sessions", "", "Authorization", adm)
	require.Equal(t, 200, rec.Code, rec.Body.String())
	goldenFile(t, "admin_sessions", "list_200.json", rec.Body.Bytes())
	var page struct {
		Data       []map[string]any `json:"data"`
		NextCursor *string          `json:"next_cursor"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &page))
	require.Len(t, page.Data, 1)
	require.Equal(t, id.String(), page.Data[0]["id"])
	require.Nil(t, page.NextCursor)

	rec = e.do(http.MethodGet, "/api/v1/admin/users/"+owner.String()+"/sessions?status=CANCELLED", "", "Authorization", adm)
	require.Equal(t, 200, rec.Code)
	require.JSONEq(t, `{"data":[],"next_cursor":null}`, rec.Body.String())
	for _, q := range []string{"status=BOGUS", "limit=0", "cursor=garbage"} {
		require.Equal(t, 400, e.do(http.MethodGet, "/api/v1/admin/users/"+owner.String()+"/sessions?"+q, "", "Authorization", adm).Code, q)
	}
}

func TestAdminTurnsWritesExactlyOneAudit(t *testing.T) {
	e, adm, owner, id := adminSessionWorld(t)
	fence := e.activate(t, id)
	done, discarded := session.TurnCompleted, session.TurnDiscarded
	e.addTurn(t, fence, id, 1, "konnichiwa", &done)
	e.addTurn(t, fence, id, 2, "oops", &discarded)
	e.addTurn(t, fence, id, 3, "still going", nil)

	rec := e.do(http.MethodGet, "/api/v1/admin/sessions/"+id.String()+"/turns", "", "Authorization", adm)
	require.Equal(t, 200, rec.Code, rec.Body.String())
	goldenFile(t, "admin_sessions", "turns_200.json", rec.Body.Bytes())
	var page struct {
		Data []map[string]any `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &page))
	require.Len(t, page.Data, 3, "DISCARDED and in-progress turns are included")
	for _, want := range []string{"tts_voice_id", "used_fallback", "generation", "error_code", "input_tokens", "llm_first_token_ms"} {
		require.Contains(t, page.Data[0], want)
	}

	rows := e.adminStore.Audit()
	require.Len(t, rows, 1)
	require.Equal(t, admin.ActionSessionTranscriptView, rows[0].Action)
	require.Equal(t, "SESSION", rows[0].TargetType)
	require.Equal(t, id.String(), rows[0].TargetID)
	require.Equal(t, owner, *rows[0].TargetUserID)

	// the other admin session endpoints add nothing
	e.do(http.MethodGet, "/api/v1/admin/sessions/"+id.String(), "", "Authorization", adm)
	e.do(http.MethodGet, "/api/v1/admin/sessions/"+id.String()+"/events", "", "Authorization", adm)
	e.do(http.MethodGet, "/api/v1/admin/users/"+owner.String()+"/sessions", "", "Authorization", adm)
	require.Len(t, e.adminStore.Audit(), 1)
}

func TestAdminTurns410And404WriteNoAudit(t *testing.T) {
	e, adm, _, id := adminSessionWorld(t)
	rec := e.do(http.MethodGet, "/api/v1/admin/sessions/"+uuid.NewString()+"/turns", "", "Authorization", adm)
	require.Equal(t, 404, rec.Code)

	s := e.sessions.Session(id)
	purged := e.clk.Now()
	s.TranscriptPurgedAt = &purged
	e.sessions.PutSession(s)
	rec = e.do(http.MethodGet, "/api/v1/admin/sessions/"+id.String()+"/turns", "", "Authorization", adm)
	require.Equal(t, 410, rec.Code)
	require.Equal(t, "TRANSCRIPT_EXPIRED", errorBody(t, rec)["code"])
	require.Empty(t, e.adminStore.Audit())
}

func TestAdminTurnsAuditFailureIs500WithoutTranscript(t *testing.T) {
	e, adm, _, id := adminSessionWorld(t)
	fence := e.activate(t, id)
	done := session.TurnCompleted
	e.addTurn(t, fence, id, 1, "secret words", &done)
	e.adminStore.FailNext("Audit.Insert", errors.New("audit down"))

	rec := e.do(http.MethodGet, "/api/v1/admin/sessions/"+id.String()+"/turns", "", "Authorization", adm)
	require.Equal(t, 500, rec.Code)
	require.NotContains(t, rec.Body.String(), "secret words")
	require.Empty(t, e.adminStore.Audit())
}

func TestAdminEventsGoldenAndCursor(t *testing.T) {
	e, adm, _, id := adminSessionWorld(t)
	e.activate(t, id)
	rec := e.do(http.MethodGet, "/api/v1/admin/sessions/"+id.String()+"/events", "", "Authorization", adm)
	require.Equal(t, 200, rec.Code, rec.Body.String())
	goldenFile(t, "admin_sessions", "events_200.json", rec.Body.Bytes())
	var all struct {
		Data []map[string]any `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &all))
	require.GreaterOrEqual(t, len(all.Data), 3)
	for _, want := range []string{"id", "event_type", "actor", "from_status", "to_status", "turn_id", "payload", "created_at"} {
		require.Contains(t, all.Data[0], want)
	}

	rec = e.do(http.MethodGet, "/api/v1/admin/sessions/"+id.String()+"/events?limit=2", "", "Authorization", adm)
	var first struct {
		Data       []map[string]any `json:"data"`
		NextCursor *string          `json:"next_cursor"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &first))
	require.Len(t, first.Data, 2)
	require.NotNil(t, first.NextCursor)
	rec = e.do(http.MethodGet, "/api/v1/admin/sessions/"+id.String()+"/events?limit=100&cursor="+*first.NextCursor, "", "Authorization", adm)
	var rest struct {
		Data []map[string]any `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rest))
	require.Len(t, rest.Data, len(all.Data)-2)
	require.Equal(t, all.Data[2]["id"], rest.Data[0]["id"])

	require.Equal(t, 400, e.do(http.MethodGet, "/api/v1/admin/sessions/"+id.String()+"/events?cursor=garbage", "", "Authorization", adm).Code)
	require.Equal(t, 400, e.do(http.MethodGet, "/api/v1/admin/sessions/"+id.String()+"/events?limit=101", "", "Authorization", adm).Code)
}
