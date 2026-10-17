package kotokiclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestAdminGrantBalance(t *testing.T) {
	uid := uuid.MustParse("01929a40-2222-7abc-8def-000000000002")
	key := "01929a40-3333-7abc-8def-000000000003"
	var calls int
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/admin/users/{id}/balance-grants", func(w http.ResponseWriter, r *http.Request) {
		calls++
		require.Equal(t, uid.String(), r.PathValue("id"))
		require.Equal(t, key, r.Header.Get("Idempotency-Key"))
		require.Equal(t, "Bearer tok-admin", r.Header.Get("Authorization"))
		var body map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		require.Equal(t, map[string]any{"seconds": float64(36000), "reason": "load test seed"}, body)
		writeJSON(w, http.StatusCreated, map[string]any{"id": "x"})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := New(srv.URL)
	c.SetAccessToken("tok-admin")
	require.NoError(t, c.AdminGrantBalance(context.Background(), uid, 36000, "load test seed", key))
	require.Equal(t, 1, calls)
}

func TestAdminGrantBalance_KeyReusedIsTyped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusConflict, map[string]any{"error": map[string]any{"code": "IDEMPOTENCY_KEY_REUSED", "message": "reused", "request_id": "q", "details": nil}})
	}))
	defer srv.Close()

	err := New(srv.URL).AdminGrantBalance(context.Background(), uuid.New(), 1, "r", uuid.NewString())
	require.ErrorIs(t, err, ErrIdempotencyKeyReused)
	var api *APIError
	require.ErrorAs(t, err, &api)
	require.Equal(t, http.StatusConflict, api.Status)

	// another 409 is a plain API error, not the typed one
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusConflict, map[string]any{"error": map[string]any{"code": "OTHER", "message": "x", "request_id": "q", "details": nil}})
	}))
	defer srv2.Close()
	err = New(srv2.URL).AdminGrantBalance(context.Background(), uuid.New(), 1, "r", uuid.NewString())
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrIdempotencyKeyReused)
}
