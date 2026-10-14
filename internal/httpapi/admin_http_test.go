package httpapi_test

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func adminPath(user uuid.UUID, tail string) string {
	return "/api/v1/admin/users/" + user.String() + tail
}

func TestAdminRBAC(t *testing.T) {
	e := newAPI(t)
	adm, _ := e.adminAuthz(t)
	userAuthz := bearerOf(t, e.register(t, "hana@example.com"))

	rec := e.do(http.MethodGet, "/api/v1/admin/users", "")
	require.Equal(t, 401, rec.Code)
	require.Equal(t, "AUTH_REQUIRED", errorBody(t, rec)["code"])
	rec = e.do(http.MethodGet, "/api/v1/admin/users", "", "Authorization", userAuthz)
	require.Equal(t, 403, rec.Code)
	require.Equal(t, "FORBIDDEN", errorBody(t, rec)["code"])
	rec = e.do(http.MethodGet, "/api/v1/admin/users", "", "Authorization", adm)
	require.Equal(t, 200, rec.Code, rec.Body.String())
}

func TestAdminListUsersValidation(t *testing.T) {
	e := newAPI(t)
	adm, _ := e.adminAuthz(t)
	for _, q := range []string{"email=ab", "status=BOGUS", "role=BOGUS", "limit=101", "cursor=garbage"} {
		rec := e.do(http.MethodGet, "/api/v1/admin/users?"+q, "", "Authorization", adm)
		require.Equal(t, 400, rec.Code, q)
	}
	e.register(t, "hana1@example.com")
	e.register(t, "hana2@example.com")
	rec := e.do(http.MethodGet, "/api/v1/admin/users?email=han", "", "Authorization", adm)
	require.Equal(t, 200, rec.Code)
	var out struct {
		Data []map[string]any `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.Len(t, out.Data, 2)
	require.Contains(t, out.Data[0], "deleted_at")
	require.Contains(t, out.Data[0], "purged_at")
	require.Contains(t, out.Data[0], "balance")
}

func TestAdminGrant(t *testing.T) {
	e := newAPI(t)
	adm, _ := e.adminAuthz(t)
	e.register(t, "hana@example.com")
	u := e.userByEmail(t, "hana@example.com")
	path := adminPath(u.ID, "/balance-grants")
	body := `{"seconds":1800,"reason":"Beta tester grant"}`
	key := "0192f1a0-1111-7000-8000-0000000000bb"

	rec := e.do(http.MethodPost, path, body, "Authorization", adm)
	require.Equal(t, "IDEMPOTENCY_KEY_REQUIRED", errorBody(t, rec)["code"])
	rec = e.do(http.MethodPost, path, body, "Authorization", adm, "Idempotency-Key", "not-a-uuid")
	require.Equal(t, "IDEMPOTENCY_KEY_INVALID", errorBody(t, rec)["code"])

	rec = e.do(http.MethodPost, path, body, "Authorization", adm, "Idempotency-Key", key)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	require.Empty(t, rec.Header().Get("Idempotent-Replayed"))
	goldenFile(t, "admin", "grant_201.json", rec.Body.Bytes())

	rec = e.do(http.MethodPost, path, body, "Authorization", adm, "Idempotency-Key", key)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "true", rec.Header().Get("Idempotent-Replayed"))
	require.Len(t, e.bal.Ledger(), 1, "the replay booked nothing")

	rec = e.do(http.MethodPost, path, `{"seconds":600,"reason":"Beta tester grant"}`, "Authorization", adm, "Idempotency-Key", key)
	require.Equal(t, 409, rec.Code)
	require.Equal(t, "IDEMPOTENCY_KEY_REUSED", errorBody(t, rec)["code"])

	rec = e.do(http.MethodPost, adminPath(uuid.New(), "/balance-grants"), body, "Authorization", adm, "Idempotency-Key", uuid.NewString())
	require.Equal(t, 404, rec.Code)
	require.Equal(t, "USER_NOT_FOUND", errorBody(t, rec)["code"])

	rec = e.do(http.MethodPost, path, `{"seconds":360001,"reason":"Beta tester grant"}`, "Authorization", adm, "Idempotency-Key", uuid.NewString())
	require.Equal(t, 400, rec.Code)
	rec = e.do(http.MethodPost, path, `{"seconds":60,"reason":"x"}`, "Authorization", adm, "Idempotency-Key", uuid.NewString())
	require.Equal(t, 400, rec.Code)
}

func TestAdminAdjust(t *testing.T) {
	e := newAPI(t)
	adm, _ := e.adminAuthz(t)
	e.register(t, "hana@example.com")
	u := e.userByEmail(t, "hana@example.com")
	grant := e.do(http.MethodPost, adminPath(u.ID, "/balance-grants"), `{"seconds":600,"reason":"initial credit"}`, "Authorization", adm, "Idempotency-Key", uuid.NewString())
	require.Equal(t, 201, grant.Code)
	path := adminPath(u.ID, "/balance-adjustments")

	rec := e.do(http.MethodPost, path, `{"seconds_delta":-999,"reason":"too much"}`, "Authorization", adm, "Idempotency-Key", uuid.NewString())
	require.Equal(t, 409, rec.Code)
	require.Equal(t, "BALANCE_ADJUSTMENT_NEGATIVE", errorBody(t, rec)["code"])
	rec = e.do(http.MethodPost, path, `{"seconds_delta":0,"reason":"nothing"}`, "Authorization", adm, "Idempotency-Key", uuid.NewString())
	require.Equal(t, 400, rec.Code)

	rec = e.do(http.MethodPost, path, `{"seconds_delta":-300,"reason":"correction"}`, "Authorization", adm, "Idempotency-Key", uuid.NewString())
	require.Equal(t, 201, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), `"entry_type":"ADJUSTMENT"`)
	require.Contains(t, rec.Body.String(), `"available_seconds":300`)
}

func TestAdminPatchStatus(t *testing.T) {
	e := newAPI(t)
	adm, adminID := e.adminAuthz(t)
	e.register(t, "hana@example.com")
	u := e.userByEmail(t, "hana@example.com")

	rec := e.do(http.MethodPatch, adminPath(u.ID, ""), `{"status":"SUSPENDED","reason":"Abuse report #123"}`, "Authorization", adm)
	require.Equal(t, 200, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), `"status":"SUSPENDED"`)

	rec = e.do(http.MethodPatch, adminPath(adminID, ""), `{"status":"SUSPENDED","reason":"Abuse report #123"}`, "Authorization", adm)
	require.Equal(t, 409, rec.Code)
	require.Equal(t, "CANNOT_MODIFY_SELF", errorBody(t, rec)["code"])

	rec = e.do(http.MethodPatch, adminPath(u.ID, ""), `{"status":"ACTIVE","reason":"x"}`, "Authorization", adm)
	require.Equal(t, 400, rec.Code)
	rec = e.do(http.MethodPatch, adminPath(u.ID, ""), `{"status":"BOGUS","reason":"valid reason"}`, "Authorization", adm)
	require.Equal(t, 400, rec.Code)
	rec = e.do(http.MethodPatch, adminPath(u.ID, ""), `{"reason":"valid reason"}`, "Authorization", adm)
	require.Equal(t, 400, rec.Code)
}

func TestAdminGetUser(t *testing.T) {
	e := newAPI(t)
	adm, _ := e.adminAuthz(t)
	e.register(t, "hana@example.com")
	u := e.userByEmail(t, "hana@example.com")
	rec := e.do(http.MethodGet, adminPath(u.ID, ""), "", "Authorization", adm)
	require.Equal(t, 200, rec.Code)
	var out map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.Contains(t, out, "open_session_id")
	require.Nil(t, out["open_session_id"])
	require.EqualValues(t, 1, out["active_refresh_token_count"])

	rec = e.do(http.MethodGet, adminPath(uuid.New(), ""), "", "Authorization", adm)
	require.Equal(t, "USER_NOT_FOUND", errorBody(t, rec)["code"])
	rec = e.do(http.MethodGet, "/api/v1/admin/users/not-a-uuid", "", "Authorization", adm)
	require.Equal(t, 404, rec.Code)
}

func TestAdminAuditLogsPagination(t *testing.T) {
	e := newAPI(t)
	adm, _ := e.adminAuthz(t)
	e.register(t, "hana@example.com")
	u := e.userByEmail(t, "hana@example.com")
	for i := 0; i < 3; i++ {
		rec := e.do(http.MethodPost, adminPath(u.ID, "/balance-grants"), `{"seconds":60,"reason":"credit `+string(rune('a'+i))+`"}`, "Authorization", adm, "Idempotency-Key", uuid.NewString())
		require.Equal(t, 201, rec.Code)
	}
	rec := e.do(http.MethodPatch, adminPath(u.ID, ""), `{"status":"SUSPENDED","reason":"abuse report"}`, "Authorization", adm)
	require.Equal(t, 200, rec.Code)

	type page struct {
		Data       []map[string]any `json:"data"`
		NextCursor *string          `json:"next_cursor"`
	}
	var p1 page
	rec = e.do(http.MethodGet, "/api/v1/admin/audit-logs?limit=3&action="+url.QueryEscape("BALANCE_GRANT"), "", "Authorization", adm)
	require.Equal(t, 200, rec.Code, rec.Body.String())
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &p1))
	require.Len(t, p1.Data, 3)
	require.Nil(t, p1.NextCursor)
	require.Equal(t, "BALANCE_GRANT", p1.Data[0]["action"])
	require.Equal(t, "USER", p1.Data[0]["target_type"])
	require.NotNil(t, p1.Data[0]["details"])

	var all page
	rec = e.do(http.MethodGet, "/api/v1/admin/audit-logs?limit=2", "", "Authorization", adm)
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &all))
	require.Len(t, all.Data, 2)
	require.NotNil(t, all.NextCursor)

	rec = e.do(http.MethodGet, "/api/v1/admin/audit-logs?target_user_id="+u.ID.String(), "", "Authorization", adm)
	require.Equal(t, 200, rec.Code)
	rec = e.do(http.MethodGet, "/api/v1/admin/audit-logs?target_user_id=zzz", "", "Authorization", adm)
	require.Equal(t, 400, rec.Code)
}

func TestAdminUserTransactionsIncludeReason(t *testing.T) {
	e := newAPI(t)
	adm, _ := e.adminAuthz(t)
	e.register(t, "hana@example.com")
	u := e.userByEmail(t, "hana@example.com")
	e.do(http.MethodPost, adminPath(u.ID, "/balance-grants"), `{"seconds":600,"reason":"Beta tester grant"}`, "Authorization", adm, "Idempotency-Key", uuid.NewString())

	rec := e.do(http.MethodGet, adminPath(u.ID, "/balance/transactions"), "", "Authorization", adm)
	require.Equal(t, 200, rec.Code, rec.Body.String())
	require.True(t, strings.Contains(rec.Body.String(), `"reason":"Beta tester grant"`))
	require.Contains(t, rec.Body.String(), `"admin_user_id"`)
	require.Contains(t, rec.Body.String(), `"idempotency_key":"grant:`)

	rec = e.do(http.MethodGet, adminPath(uuid.New(), "/balance/transactions"), "", "Authorization", adm)
	require.Equal(t, 404, rec.Code)
}
