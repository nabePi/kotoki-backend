package httpapi_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/auth"
	"github.com/nabePi/kotoki-backend/internal/platform/errs"
)

func TestMeGolden(t *testing.T) {
	e := newAPI(t)
	authz := bearerOf(t, e.register(t, "hana@example.com"))
	rec := e.do(http.MethodGet, "/api/v1/me", "", "Authorization", authz)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
	goldenFile(t, "me", "me_200.json", rec.Body.Bytes())

	noToken := e.do(http.MethodGet, "/api/v1/me", "")
	require.Equal(t, 401, noToken.Code)
	require.Equal(t, "AUTH_REQUIRED", errorBody(t, noToken)["code"])
}

func TestConsents(t *testing.T) {
	e := newAPI(t)
	reg := e.register(t, "hana@example.com")
	authz := bearerOf(t, reg)

	rec := e.do(http.MethodPut, "/api/v1/me/consents", `{}`, "Authorization", authz)
	require.Equal(t, 400, rec.Code)
	require.Equal(t, map[string]any{"fields": []any{map[string]any{"field": "terms_version", "reason": "required"}}}, errorBody(t, rec)["details"])

	rec = e.do(http.MethodPut, "/api/v1/me/consents", `{"terms_version":"1999-01-01"}`, "Authorization", authz)
	require.Equal(t, 400, rec.Code)
	require.Equal(t, map[string]any{"fields": []any{map[string]any{"field": "terms_version", "reason": "not_current"}}}, errorBody(t, rec)["details"])

	rec = e.do(http.MethodPut, "/api/v1/me/consents", `{"terms_version":"`+apiTerms+`"}`, "Authorization", authz)
	require.Equal(t, 200, rec.Code)
	require.Contains(t, rec.Body.String(), `"terms_current_version":"`+apiTerms+`"`)
}

func TestDeleteMe(t *testing.T) {
	e := newAPI(t)
	reg := e.register(t, "hana@example.com")
	authz := bearerOf(t, reg)

	rec := e.do(http.MethodDelete, "/api/v1/me", `{}`, "Authorization", authz)
	require.Equal(t, 400, rec.Code)
	rec = e.do(http.MethodDelete, "/api/v1/me", `{"password":"wrong password!!"}`, "Authorization", authz)
	require.Equal(t, 401, rec.Code)
	require.Equal(t, "INVALID_CREDENTIALS", errorBody(t, rec)["code"])
	require.Empty(t, e.deleter.calls)

	rec = e.do(http.MethodDelete, "/api/v1/me", `{"password":"correct horse battery"}`, "Authorization", authz)
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	require.JSONEq(t, `{"status":"DELETION_SCHEDULED"}`, rec.Body.String())
	require.Less(t, refreshCookie(t, rec).MaxAge, 0)
	require.Len(t, e.deleter.calls, 1)

	e.deleter.err = auth.ErrAccountHasActiveSession
	rec = e.do(http.MethodDelete, "/api/v1/me", `{"password":"correct horse battery"}`, "Authorization", authz)
	require.NotEqual(t, http.StatusAccepted, rec.Code)
}

func TestDeleteMeActiveSessionIs409(t *testing.T) {
	e := newAPI(t)
	authz := bearerOf(t, e.register(t, "hana@example.com"))
	e.deleter.err = errs.New(errs.CodeAccountHasActiveSession, "x")
	rec := e.do(http.MethodDelete, "/api/v1/me", `{"password":"correct horse battery"}`, "Authorization", authz)
	require.Equal(t, 409, rec.Code)
	require.Equal(t, "ACCOUNT_HAS_ACTIVE_SESSION", errorBody(t, rec)["code"])
}

func TestMeRequiresActiveForConsents(t *testing.T) {
	e := newAPI(t)
	reg := e.register(t, "hana@example.com")
	authz := bearerOf(t, reg)
	var uid = e.userID(t)
	e.store.MutateUser(uid, func(u *auth.User) { u.Status = auth.UserStatusSuspended })
	rec := e.do(http.MethodPut, "/api/v1/me/consents", `{"terms_version":"`+apiTerms+`"}`, "Authorization", authz)
	require.Equal(t, 403, rec.Code)
	require.Equal(t, "ACCOUNT_SUSPENDED", errorBody(t, rec)["code"])
	rec = e.do(http.MethodGet, "/api/v1/me", "", "Authorization", authz)
	require.Equal(t, 200, rec.Code, "GET /me stays readable while suspended")
}
