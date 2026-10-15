package kotokiclient

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

const refreshPath = "/api/v1/auth"

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func authBody(token string) map[string]any {
	return map[string]any{
		"access_token": token, "token_type": "Bearer", "expires_in": 900,
		"user": map[string]any{"id": "u-1", "email": "a@example.com", "full_name": "A", "role": "USER", "status": "ACTIVE", "terms_version": "2026-10"},
	}
}

func setRefresh(w http.ResponseWriter, v string) {
	http.SetCookie(w, &http.Cookie{Name: "kotoki_refresh", Value: v, Path: refreshPath, HttpOnly: true, MaxAge: 3600})
}

func TestRESTRoundTrip(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	sessionID := uuid.MustParse("01929a40-1111-7abc-8def-000000000001")
	var createKey, createAuth string
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/auth/register", func(w http.ResponseWriter, r *http.Request) {
		var in RegisterRequest
		require.NoError(t, json.NewDecoder(r.Body).Decode(&in))
		require.Equal(t, "a@example.com", in.Email)
		require.Equal(t, "2026-10", in.TermsVersion)
		require.Empty(t, r.Header.Get("Authorization"))
		setRefresh(w, "r1")
		writeJSON(w, 201, authBody("tok-register"))
	})
	mux.HandleFunc("POST /api/v1/auth/login", func(w http.ResponseWriter, r *http.Request) {
		var in struct{ Email, Password string }
		require.NoError(t, json.NewDecoder(r.Body).Decode(&in))
		require.Equal(t, "a@example.com", in.Email)
		require.Equal(t, "pw", in.Password)
		setRefresh(w, "r2")
		writeJSON(w, 200, authBody("tok-login"))
	})
	mux.HandleFunc("POST /api/v1/auth/refresh", func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie("kotoki_refresh")
		if err != nil || c.Value != "r2" {
			writeJSON(w, 401, map[string]any{"error": map[string]any{"code": "REFRESH_TOKEN_INVALID", "message": "no", "request_id": "q", "details": nil}})
			return
		}
		setRefresh(w, "r3")
		writeJSON(w, 200, map[string]any{"access_token": "tok-refresh", "token_type": "Bearer", "expires_in": 900})
	})
	mux.HandleFunc("POST /api/v1/sessions", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		createKey, createAuth = r.Header.Get("Idempotency-Key"), r.Header.Get("Authorization")
		mu.Unlock()
		require.Equal(t, "application/json", r.Header.Get("Content-Type"))
		var in map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&in))
		require.Equal(t, 300.0, in["duration_seconds"])
		require.Equal(t, "USER_FIRST", in["conversation_start_mode"])
		require.NotContains(t, in, "IdempotencyKey", "the key travels in the header only")
		writeJSON(w, 201, map[string]any{"id": sessionID, "status": "RESERVED", "duration_seconds": 300})
	})
	mux.HandleFunc("POST /api/v1/sessions/{id}/realtime-ticket", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Get("Authorization"))
		mu.Unlock()
		require.Equal(t, sessionID.String(), r.PathValue("id"))
		writeJSON(w, 200, map[string]any{"session_id": sessionID, "ticket": "tkt-abc", "expires_in": 30, "realtime_url": "ws://x"})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	ctx := context.Background()
	c := New(srv.URL)

	u, err := c.Register(ctx, RegisterRequest{Email: "a@example.com", Password: "pw", FullName: "A", TermsVersion: "2026-10"})
	require.NoError(t, err)
	require.Equal(t, "u-1", u.ID)
	require.Equal(t, "tok-register", c.AccessToken())

	require.NoError(t, c.Login(ctx, "a@example.com", "pw"))
	require.Equal(t, "tok-login", c.AccessToken())

	// The refresh cookie from Login is in the jar: Refresh sends it and gets a new token.
	require.NoError(t, c.Refresh(ctx))
	require.Equal(t, "tok-refresh", c.AccessToken())

	s, err := c.CreateSession(ctx, CreateSessionRequest{
		TopicID: uuid.New(), ScenarioID: uuid.New(), CharacterID: uuid.New(), DurationSeconds: 300, ConversationStartMode: "USER_FIRST",
	})
	require.NoError(t, err)
	require.Equal(t, sessionID, s.ID)
	require.Equal(t, "RESERVED", s.Status)
	_, err = uuid.Parse(createKey)
	require.NoError(t, err, "Idempotency-Key must be a UUID, got %q", createKey)
	require.Equal(t, "Bearer tok-refresh", createAuth)

	tk, err := c.RealtimeTicket(ctx, sessionID)
	require.NoError(t, err)
	require.Equal(t, "tkt-abc", tk)
	require.Equal(t, []string{"Bearer tok-refresh"}, seen)
}

func TestCreateSessionKeys(t *testing.T) {
	var keys []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		keys = append(keys, r.Header.Get("Idempotency-Key"))
		writeJSON(w, 201, map[string]any{"id": uuid.New(), "status": "RESERVED"})
	}))
	defer srv.Close()
	c := New(srv.URL)
	ctx := context.Background()
	_, err := c.CreateSession(ctx, CreateSessionRequest{})
	require.NoError(t, err)
	_, err = c.CreateSession(ctx, CreateSessionRequest{})
	require.NoError(t, err)
	_, err = c.CreateSession(ctx, CreateSessionRequest{IdempotencyKey: "fixed-key"})
	require.NoError(t, err)
	require.Len(t, keys, 3)
	require.NotEqual(t, keys[0], keys[1], "each call without a key gets a fresh one")
	require.Equal(t, "fixed-key", keys[2])
}

func TestErrorDecoding(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/balance":
			writeJSON(w, 402, map[string]any{"error": map[string]any{
				"code": "BALANCE_INSUFFICIENT", "message": "Saldo tidak cukup.", "request_id": "req-9",
				"details": map[string]any{"fields": []any{map[string]any{"field": "duration_seconds", "reason": "too_long"}}},
			}})
		default:
			http.Error(w, "<html>bad gateway</html>", http.StatusBadGateway)
		}
	}))
	defer srv.Close()
	c := New(srv.URL)

	_, err := c.Balance(context.Background())
	var ae *APIError
	require.ErrorAs(t, err, &ae)
	require.Equal(t, 402, ae.Status)
	require.Equal(t, "BALANCE_INSUFFICIENT", ae.Code)
	require.Equal(t, "Saldo tidak cukup.", ae.Message)
	require.Equal(t, "req-9", ae.RequestID)
	require.Contains(t, string(ae.Details), "duration_seconds")
	require.Contains(t, err.Error(), "BALANCE_INSUFFICIENT")

	// A body that is not the D-013 envelope still yields an APIError with the status.
	_, err = c.GetSession(context.Background(), uuid.New())
	require.ErrorAs(t, err, &ae)
	require.Equal(t, 502, ae.Status)
	require.Empty(t, ae.Code)
	require.Contains(t, ae.Message, "bad gateway")
}

func TestBadBaseURL(t *testing.T) {
	_, err := New("://nope").Balance(context.Background())
	require.Error(t, err)
	_, err = New("").Balance(context.Background())
	require.Error(t, err)
}

func TestCatalogAndReads(t *testing.T) {
	topic, scen, char := uuid.New(), uuid.New(), uuid.New()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "Bearer t", r.Header.Get("Authorization"))
		switch r.URL.Path {
		case "/api/v1/topics":
			writeJSON(w, 200, map[string]any{"items": []any{map[string]any{"id": topic, "code": "cafe", "name": "Cafe", "name_ja": "カフェ", "scenario_count": 2}}})
		case "/api/v1/scenarios":
			writeJSON(w, 200, map[string]any{"items": []any{map[string]any{"id": scen, "topic_id": topic, "code": "order", "difficulty_level": 2, "formality": "POLITE", "tasks": []string{"a"}, "unknown_field": 1}}})
		case "/api/v1/characters":
			writeJSON(w, 200, map[string]any{"items": []any{map[string]any{"id": char, "code": "yui", "name": "Yui", "gender": "FEMALE", "accent": "STANDARD"}}})
		case "/api/v1/balance":
			writeJSON(w, 200, map[string]any{"available_seconds": 3600, "reserved_seconds": 300, "allowed_durations": []int{300, 600}, "updated_at": "x"})
		case "/api/v1/sessions/" + scen.String() + "/cancel":
			require.Equal(t, http.MethodPost, r.Method)
			writeJSON(w, 200, map[string]any{"id": scen, "status": "CANCELLED"})
		case "/api/v1/sessions/" + scen.String() + "/evaluation":
			writeJSON(w, 200, map[string]any{"status": "COMPLETED", "score": 4})
		default:
			t.Errorf("unexpected %s", r.URL.Path)
		}
	}))
	defer srv.Close()
	c := New(srv.URL + "/") // trailing slash tolerated
	c.SetAccessToken("t")
	ctx := context.Background()

	cat, err := c.Catalog(ctx)
	require.NoError(t, err)
	require.Len(t, cat.Topics, 1)
	require.Equal(t, topic, cat.Topics[0].ID)
	require.Equal(t, "カフェ", cat.Topics[0].NameJA)
	require.Equal(t, scen, cat.Scenarios[0].ID)
	require.Equal(t, "POLITE", cat.Scenarios[0].Formality)
	require.Equal(t, 2, cat.Scenarios[0].DifficultyLevel)
	require.Equal(t, "Yui", cat.Characters[0].Name)

	b, err := c.Balance(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(3600), b.AvailableSeconds)
	require.Equal(t, []int64{300, 600}, b.AllowedDurations)

	require.NoError(t, c.CancelSession(ctx, scen))
	ev, err := c.GetEvaluation(ctx, scen)
	require.NoError(t, err)
	require.Equal(t, "COMPLETED", ev.Status)
	require.JSONEq(t, `{"status":"COMPLETED","score":4}`, string(ev.Raw))
}

func TestTurnsPagination(t *testing.T) {
	id := uuid.New()
	next := "next-cursor-xyz"
	var gotQuery map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/v1/sessions/"+id.String()+"/turns", r.URL.Path)
		gotQuery = map[string]string{"limit": r.URL.Query().Get("limit"), "cursor": r.URL.Query().Get("cursor")}
		nc := &next
		if r.URL.Query().Get("cursor") != "" {
			nc = nil
		}
		ut := "こんにちは"
		writeJSON(w, 200, map[string]any{"data": []any{map[string]any{"id": uuid.New(), "sequence_number": 4, "status": "COMPLETED", "user_text": ut}}, "next_cursor": nc})
	}))
	defer srv.Close()
	c := New(srv.URL)
	ctx := context.Background()

	turns, nc, err := c.Turns(ctx, id, nil, 0)
	require.NoError(t, err)
	require.Equal(t, map[string]string{"limit": "", "cursor": ""}, gotQuery)
	require.Len(t, turns, 1)
	require.Equal(t, 4, turns[0].SequenceNumber)
	require.Equal(t, "こんにちは", *turns[0].UserText)
	require.Equal(t, next, nc)

	after := 7
	_, nc, err = c.Turns(ctx, id, &after, 25)
	require.NoError(t, err)
	require.Empty(t, nc, "no next page")
	require.Equal(t, "25", gotQuery["limit"])
	// The cursor is the server's opaque {"v":1,"k":<seq>} in base64url without padding.
	raw, err := base64.RawURLEncoding.DecodeString(gotQuery["cursor"])
	require.NoError(t, err)
	require.JSONEq(t, `{"v":1,"k":7}`, string(raw))
	require.False(t, strings.ContainsAny(gotQuery["cursor"], "=+/"))
}
