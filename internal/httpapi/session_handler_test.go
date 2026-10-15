package httpapi_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/auth"
	"github.com/nabePi/kotoki-backend/internal/content"
	"github.com/nabePi/kotoki-backend/internal/session"
)

// sessionWorld seeds the content, registers a funded user and returns the Authorization header.
func sessionWorld(t *testing.T, seconds int64) (*apiEnv, string) {
	t.Helper()
	e := newAPI(t)
	e.seedContent()
	return e, e.fundedUser(t, "hana@example.com", seconds)
}

func createBody() string {
	return `{"topic_id":"` + cTopicID.String() + `","scenario_id":"` + cScenarioID.String() + `","character_id":"` + cCharacterID.String() +
		`","duration_seconds":900,"conversation_start_mode":"USER_FIRST"}`
}

func (e *apiEnv) createSession(t *testing.T, authz, key string) *httptest.ResponseRecorder {
	t.Helper()
	return e.do(http.MethodPost, "/api/v1/sessions", createBody(), "Authorization", authz, "Idempotency-Key", key)
}

func TestSessionCreateGoldenAndReplay(t *testing.T) {
	e, authz := sessionWorld(t, 1800)
	rec := e.createSession(t, authz, "key-0000001")
	require.Equal(t, 201, rec.Code, rec.Body.String())
	var created struct {
		ID     uuid.UUID `json:"id"`
		Status string    `json:"status"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))
	require.Equal(t, "RESERVED", created.Status)
	require.Equal(t, "/api/v1/sessions/"+created.ID.String(), rec.Header().Get("Location"))
	goldenFile(t, "sessions", "create_201.json", rec.Body.Bytes())

	again := e.createSession(t, authz, "key-0000001")
	require.Equal(t, 200, again.Code, again.Body.String())
	require.Equal(t, "true", again.Header().Get("Idempotent-Replayed"))
	require.Empty(t, again.Header().Get("Location"))
	var replay struct {
		ID uuid.UUID `json:"id"`
	}
	require.NoError(t, json.Unmarshal(again.Body.Bytes(), &replay))
	require.Equal(t, created.ID, replay.ID)
}

func TestSessionGetShapeAndNameSources(t *testing.T) {
	e, authz := sessionWorld(t, 1800)
	var created struct{ ID uuid.UUID }
	require.NoError(t, json.Unmarshal(e.createSession(t, authz, "key-0000001").Body.Bytes(), &created))

	// the live character is disabled and renamed after the session was created: the user view keeps the snapshot
	e.content.PutCharacter(content.Character{ID: cCharacterID, Code: "HARUKA", Status: content.StatusDisabled, Name: "Renamed", Persona: strings.Repeat("x", 60),
		ProviderName: "ELEVENLABS", ModelName: "m", VoiceID: "v", Voice: content.Voice{Gender: content.GenderFemale, Accent: content.AccentKanto, SpeakingRate: 1}})
	rec := e.do(http.MethodGet, "/api/v1/sessions/"+created.ID.String(), "", "Authorization", authz)
	require.Equal(t, 200, rec.Code, rec.Body.String())
	goldenFile(t, "sessions", "get_200.json", rec.Body.Bytes())
	var m map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &m))
	for _, banned := range []string{"title", "avatar_url", "adaptive_difficulty", "runtime_fence", "config_snapshot", "opening_prompt_version_id", "voice_id", "provider_name"} {
		require.NotContains(t, m, banned)
	}
	require.Equal(t, "Backend Engineer Interview", m["scenario"].(map[string]any)["name"])
	require.Equal(t, "Job Interview", m["topic"].(map[string]any)["name"])
	character := m["character"].(map[string]any)
	require.Equal(t, "Haruka", character["name"])
	require.Equal(t, "haruka", character["avatar_key"])
	require.Nil(t, m["remaining_seconds"], "only an ACTIVE session has remaining time")
	require.NotNil(t, m["server_time"])
	require.NotContains(t, rec.Body.String(), "21m00Tcm4TlvDq8ikWAM", "vendor voice ids never reach the user")
}

func TestSessionRemainingSecondsWhenActive(t *testing.T) {
	e, authz := sessionWorld(t, 1800)
	var created struct{ ID uuid.UUID }
	require.NoError(t, json.Unmarshal(e.createSession(t, authz, "key-0000001").Body.Bytes(), &created))
	ctx := t.Context()
	fence, _, err := e.sessionSvc.ClaimRuntime(ctx, created.ID, []session.Status{session.StatusReserved})
	require.NoError(t, err)
	_, err = e.sessionSvc.BeginStart(ctx, created.ID, fence)
	require.NoError(t, err)
	_, err = e.sessionSvc.Activate(ctx, created.ID, fence)
	require.NoError(t, err)
	e.clk.Advance(100*time.Second + 400*time.Millisecond)

	rec := e.do(http.MethodGet, "/api/v1/sessions/"+created.ID.String(), "", "Authorization", authz)
	var m struct {
		Status    string `json:"status"`
		Remaining *int   `json:"remaining_seconds"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &m))
	require.Equal(t, "ACTIVE", m.Status)
	require.NotNil(t, m.Remaining)
	require.Equal(t, 800, *m.Remaining, "ceil(900 − 100.4)")
}

func TestSessionListEnvelopeFilterAndCursor(t *testing.T) {
	e, authz := sessionWorld(t, 5000)
	var ids []uuid.UUID
	for _, key := range []string{"key-0000001", "key-0000002", "key-0000003"} {
		var c struct{ ID uuid.UUID }
		require.NoError(t, json.Unmarshal(e.createSession(t, authz, key).Body.Bytes(), &c))
		ids = append(ids, c.ID)
		if key != "key-0000003" {
			rec := e.do(http.MethodPost, "/api/v1/sessions/"+c.ID.String()+"/cancel", "", "Authorization", authz)
			require.Equal(t, 200, rec.Code, rec.Body.String())
		}
	}
	rec := e.do(http.MethodGet, "/api/v1/sessions?limit=2", "", "Authorization", authz)
	require.Equal(t, 200, rec.Code, rec.Body.String())
	goldenFile(t, "sessions", "list_200.json", rec.Body.Bytes())
	var page struct {
		Data       []map[string]any `json:"data"`
		NextCursor *string          `json:"next_cursor"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &page))
	require.Len(t, page.Data, 2)
	require.NotNil(t, page.NextCursor)
	require.Equal(t, ids[2].String(), page.Data[0]["id"], "id DESC")
	for _, banned := range []string{"adaptive_difficulty", "config_snapshot"} {
		require.NotContains(t, page.Data[0], banned)
	}

	rec = e.do(http.MethodGet, "/api/v1/sessions?limit=2&cursor="+*page.NextCursor, "", "Authorization", authz)
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &page))
	require.Len(t, page.Data, 1)
	require.Nil(t, page.NextCursor)

	rec = e.do(http.MethodGet, "/api/v1/sessions?status=CANCELLED", "", "Authorization", authz)
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &page))
	require.Len(t, page.Data, 2)
	rec = e.do(http.MethodGet, "/api/v1/sessions?status=RESERVED,CANCELLED", "", "Authorization", authz)
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &page))
	require.Len(t, page.Data, 3)

	rec = e.do(http.MethodGet, "/api/v1/sessions?limit=101", "", "Authorization", authz)
	require.Equal(t, 400, rec.Code)
	require.Equal(t, "VALIDATION_FAILED", codeOf(t, rec))
	rec = e.do(http.MethodGet, "/api/v1/sessions?cursor=%25%25", "", "Authorization", authz)
	require.Equal(t, 400, rec.Code)
	require.Equal(t, "INVALID_CURSOR", codeOf(t, rec))
	rec = e.do(http.MethodGet, "/api/v1/sessions?status=PAUSED", "", "Authorization", authz)
	require.Equal(t, 400, rec.Code)
}

func TestSessionCreateErrors(t *testing.T) {
	e, authz := sessionWorld(t, 1800)
	post := func(body string, headers ...string) *httptest.ResponseRecorder {
		return e.do(http.MethodPost, "/api/v1/sessions", body, append([]string{"Authorization", authz}, headers...)...)
	}
	require.Equal(t, "IDEMPOTENCY_KEY_REQUIRED", codeOf(t, post(createBody())))
	require.Equal(t, "IDEMPOTENCY_KEY_INVALID", codeOf(t, post(createBody(), "Idempotency-Key", "short")))

	for _, extra := range []string{"version_id", "conversation_style_id", "expression_profile_id", "voice_id", "provider"} {
		body := strings.TrimSuffix(createBody(), "}") + `,"` + extra + `":"` + uuid.NewString() + `"}`
		rec := post(body, "Idempotency-Key", "key-0000001")
		require.Equal(t, 400, rec.Code, extra)
		require.Equal(t, "VALIDATION_FAILED", codeOf(t, rec), extra)
		require.Contains(t, rec.Body.String(), "unknown_field", extra)
	}
	for name, body := range map[string]string{
		"missing topic":   `{"scenario_id":"` + cScenarioID.String() + `","character_id":"` + cCharacterID.String() + `","duration_seconds":900,"conversation_start_mode":"USER_FIRST"}`,
		"bad uuid":        strings.Replace(createBody(), cTopicID.String(), "nope", 1),
		"bad mode":        strings.Replace(createBody(), "USER_FIRST", "BOTH", 1),
		"no duration":     strings.Replace(createBody(), `"duration_seconds":900,`, "", 1),
		"duration 7 s":    strings.Replace(createBody(), "900", "7", 1),
		"empty json body": `{}`,
	} {
		rec := post(body, "Idempotency-Key", "key-0000001")
		require.Equal(t, 400, rec.Code, name+": "+rec.Body.String())
		require.Equal(t, "VALIDATION_FAILED", codeOf(t, rec), name)
	}

	other := strings.Replace(createBody(), cScenarioID.String(), uuid.NewString(), 1)
	rec := post(other, "Idempotency-Key", "key-0000001")
	require.Equal(t, 422, rec.Code)
	require.Equal(t, "SCENARIO_NOT_AVAILABLE", codeOf(t, rec))

	require.Equal(t, 201, post(createBody(), "Idempotency-Key", "key-0000002").Code)
	rec = post(createBody(), "Idempotency-Key", "key-0000003")
	require.Equal(t, 409, rec.Code)
	require.Equal(t, "SESSION_ALREADY_OPEN", codeOf(t, rec))
	rec = post(strings.Replace(createBody(), "900", "600", 1), "Idempotency-Key", "key-0000002")
	require.Equal(t, 409, rec.Code)
	require.Equal(t, "IDEMPOTENCY_KEY_REUSED", codeOf(t, rec))
}

func TestSessionBalanceInsufficient402(t *testing.T) {
	e, authz := sessionWorld(t, 300)
	rec := e.createSession(t, authz, "key-0000001")
	require.Equal(t, 402, rec.Code)
	require.Equal(t, "BALANCE_INSUFFICIENT", codeOf(t, rec))
	require.Contains(t, rec.Body.String(), `"available_seconds":300`)
	require.Contains(t, rec.Body.String(), `"requested_seconds":900`)
}

func TestSessionNotFoundCases(t *testing.T) {
	e, authz := sessionWorld(t, 1800)
	var created struct{ ID uuid.UUID }
	require.NoError(t, json.Unmarshal(e.createSession(t, authz, "key-0000001").Body.Bytes(), &created))
	other := bearerOf(t, e.register(t, "other@example.com"))

	for _, c := range []struct{ method, path string }{
		{http.MethodGet, "/sessions/not-a-uuid"}, {http.MethodGet, "/sessions/" + uuid.NewString()},
		{http.MethodPost, "/sessions/not-a-uuid/cancel"}, {http.MethodPost, "/sessions/" + uuid.NewString() + "/realtime-ticket"},
		{http.MethodGet, "/sessions/not-a-uuid/turns"},
	} {
		rec := e.do(c.method, "/api/v1"+c.path, "", "Authorization", authz)
		require.Equal(t, 404, rec.Code, c.path)
		require.Equal(t, "SESSION_NOT_FOUND", codeOf(t, rec), c.path)
	}
	for _, c := range []struct{ method, path string }{
		{http.MethodGet, "/sessions/" + created.ID.String()}, {http.MethodPost, "/sessions/" + created.ID.String() + "/cancel"},
		{http.MethodGet, "/sessions/" + created.ID.String() + "/turns"},
	} {
		rec := e.do(c.method, "/api/v1"+c.path, "", "Authorization", other)
		require.Equal(t, 404, rec.Code, "another user's session: "+c.path)
		require.Equal(t, "SESSION_NOT_FOUND", codeOf(t, rec))
	}
}

func TestSessionCancelIdempotentAndNotCancellable(t *testing.T) {
	e, authz := sessionWorld(t, 1800)
	var created struct{ ID uuid.UUID }
	require.NoError(t, json.Unmarshal(e.createSession(t, authz, "key-0000001").Body.Bytes(), &created))
	path := "/api/v1/sessions/" + created.ID.String() + "/cancel"

	first := e.do(http.MethodPost, path, "", "Authorization", authz)
	require.Equal(t, 200, first.Code, first.Body.String())
	require.Contains(t, first.Body.String(), `"status":"CANCELLED"`)
	second := e.do(http.MethodPost, path, "", "Authorization", authz)
	require.Equal(t, 200, second.Code)
	require.Equal(t, first.Body.String(), second.Body.String(), "idempotent")

	var c2 struct{ ID uuid.UUID }
	require.NoError(t, json.Unmarshal(e.createSession(t, authz, "key-0000002").Body.Bytes(), &c2))
	fence, _, err := e.sessionSvc.ClaimRuntime(t.Context(), c2.ID, []session.Status{session.StatusReserved})
	require.NoError(t, err)
	_, err = e.sessionSvc.BeginStart(t.Context(), c2.ID, fence)
	require.NoError(t, err)
	rec := e.do(http.MethodPost, "/api/v1/sessions/"+c2.ID.String()+"/cancel", "", "Authorization", authz)
	require.Equal(t, 409, rec.Code)
	require.Equal(t, "SESSION_NOT_CANCELLABLE", codeOf(t, rec))
	require.Contains(t, rec.Body.String(), `"status":"STARTING"`)
}

func TestSessionTicket(t *testing.T) {
	e, authz := sessionWorld(t, 1800)
	var created struct{ ID uuid.UUID }
	require.NoError(t, json.Unmarshal(e.createSession(t, authz, "key-0000001").Body.Bytes(), &created))
	user := e.userByEmail(t, "hana@example.com")
	path := "/api/v1/sessions/" + created.ID.String() + "/realtime-ticket"

	e.ticketTargets.Set(created.ID, user.ID, auth.TicketTarget{Status: "RESERVED", StartDeadlineAt: e.clk.Now().Add(5 * time.Minute)})
	rec := e.do(http.MethodPost, path, "", "Authorization", authz)
	require.Equal(t, 200, rec.Code, rec.Body.String())
	var out struct {
		SessionID   uuid.UUID `json:"session_id"`
		Ticket      string    `json:"ticket"`
		ExpiresIn   int       `json:"expires_in"`
		RealtimeURL string    `json:"realtime_url"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.Equal(t, created.ID, out.SessionID)
	require.NotEmpty(t, out.Ticket)
	require.Equal(t, 30, out.ExpiresIn)
	require.Equal(t, "https://realtime.example/api/v1/realtime", out.RealtimeURL)

	e.ticketTargets.Set(created.ID, user.ID, auth.TicketTarget{Status: "RESERVED", StartDeadlineAt: e.clk.Now().Add(-time.Second)})
	rec = e.do(http.MethodPost, path, "", "Authorization", authz)
	require.Equal(t, 409, rec.Code)
	require.Equal(t, "SESSION_START_DEADLINE_PASSED", codeOf(t, rec))

	e.ticketTargets.Set(created.ID, user.ID, auth.TicketTarget{Status: "COMPLETED"})
	rec = e.do(http.MethodPost, path, "", "Authorization", authz)
	require.Equal(t, 409, rec.Code)
	require.Equal(t, "SESSION_NOT_CONNECTABLE", codeOf(t, rec))
	require.Contains(t, rec.Body.String(), `"status":"COMPLETED"`)
}

func TestSessionTurnsEnvelopeAnd410(t *testing.T) {
	e, authz := sessionWorld(t, 1800)
	var created struct{ ID uuid.UUID }
	require.NoError(t, json.Unmarshal(e.createSession(t, authz, "key-0000001").Body.Bytes(), &created))
	ctx := t.Context()
	fence, _, err := e.sessionSvc.ClaimRuntime(ctx, created.ID, []session.Status{session.StatusReserved})
	require.NoError(t, err)
	_, err = e.sessionSvc.BeginStart(ctx, created.ID, fence)
	require.NoError(t, err)
	started, err := e.sessionSvc.Activate(ctx, created.ID, fence)
	require.NoError(t, err)
	for seq := 1; seq <= 3; seq++ {
		turnID := uuid.MustParse(fmt.Sprintf("0199c82c-d000-7000-8000-%012d", seq)) // fixed ids keep the golden file stable
		require.NoError(t, e.sessions.WithinTx(ctx, func(ctx context.Context, tx session.Tx) error {
			require.NoError(t, tx.Turns().Insert(ctx, fence, session.TurnStart{ID: turnID, SessionID: created.ID, Sequence: seq, Generation: 1, CreatedAt: *started.StartedAt}))
			text, reply := "こんにちは", "はじめまして"
			require.NoError(t, tx.Turns().UpdateUser(ctx, fence, turnID, session.TurnUser{CreatedAt: *started.StartedAt, Text: text}))
			return tx.Turns().Finalize(ctx, fence, turnID, session.TurnFinal{CreatedAt: *started.StartedAt, Status: session.TurnCompleted, AssistantText: &reply,
				AssistantSpokenText: &reply, CompletedAt: *started.StartedAt})
		}))
	}
	rec := e.do(http.MethodGet, "/api/v1/sessions/"+created.ID.String()+"/turns?limit=2", "", "Authorization", authz)
	require.Equal(t, 200, rec.Code, rec.Body.String())
	goldenFile(t, "sessions", "turns_200.json", rec.Body.Bytes())
	var page struct {
		Data       []map[string]any `json:"data"`
		NextCursor *string          `json:"next_cursor"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &page))
	require.Len(t, page.Data, 2)
	require.NotNil(t, page.NextCursor)
	require.EqualValues(t, 1, page.Data[0]["sequence_number"])
	require.Equal(t, "COMPLETED", page.Data[0]["status"])
	require.Equal(t, "はじめまして", page.Data[0]["assistant_spoken_text"])
	rec = e.do(http.MethodGet, "/api/v1/sessions/"+created.ID.String()+"/turns?limit=2&cursor="+*page.NextCursor, "", "Authorization", authz)
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &page))
	require.Len(t, page.Data, 1)
	require.Nil(t, page.NextCursor)
	rec = e.do(http.MethodGet, "/api/v1/sessions/"+created.ID.String()+"/turns?limit=101", "", "Authorization", authz)
	require.Equal(t, 400, rec.Code)

	// finish the session, then let the retention pass
	_, err = e.sessionSvc.BeginEnding(ctx, created.ID, &fence, session.EndUserEnded, "", session.ActorRealtime)
	require.NoError(t, err)
	done, err := e.sessionSvc.Finalize(ctx, created.ID, &fence, session.ActorRealtime)
	require.NoError(t, err)
	e.clk.Set(done.EndedAt.Add(181 * 24 * time.Hour))
	login := e.do(http.MethodPost, "/api/v1/auth/login", `{"email":"hana@example.com","password":"correct horse battery"}`)
	require.Equal(t, 200, login.Code, login.Body.String())
	authz = bearerOf(t, login) // the old access token expired long ago
	rec = e.do(http.MethodGet, "/api/v1/sessions/"+created.ID.String()+"/turns", "", "Authorization", authz)
	require.Equal(t, 410, rec.Code, rec.Body.String())
	require.Equal(t, "TRANSCRIPT_EXPIRED", codeOf(t, rec))
}
