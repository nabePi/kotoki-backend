package httpapi_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/httpapi"
	"github.com/nabePi/kotoki-backend/internal/platform/errs"
)

type createReq struct {
	Name string  `json:"name"`
	Note *string `json:"note"`
}

func (c createReq) Validate() error {
	if c.Name == "bad" {
		return httpapi.FieldError("name", "not_allowed")
	}
	return nil
}

type createResp struct {
	Name string  `json:"name"`
	Note *string `json:"note"`
}

const testRequestID = "0192f1a0-1111-7000-8000-000000000001"

func do(h http.Handler, method, body, contentType string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "/x", strings.NewReader(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	req = req.WithContext(httpapi.WithRequestID(req.Context(), testRequestID))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func echo(_ context.Context, req httpapi.Request[createReq]) (createResp, error) {
	return createResp{Name: req.Body.Name, Note: req.Body.Note}, nil
}

func errorBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out struct {
		Error map[string]any `json:"error"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	return out.Error
}

func TestHandleDecodeAndStatus(t *testing.T) {
	h := httpapi.Handle(http.StatusCreated, echo)
	rec := do(h, http.MethodPost, `{"name":"hana"}`, "application/json")
	require.Equal(t, http.StatusCreated, rec.Code)
	require.Equal(t, "application/json; charset=utf-8", rec.Header().Get("Content-Type"))
	require.JSONEq(t, `{"name":"hana","note":null}`, rec.Body.String())

	rec = do(h, http.MethodPost, `{"name":"hana"}`, "application/json; charset=utf-8")
	require.Equal(t, http.StatusCreated, rec.Code)
}

func TestHandleErrors(t *testing.T) {
	h := httpapi.Handle(http.StatusCreated, echo)
	cases := []struct {
		name, body, ct string
		status         int
		code           string
		details        any
	}{
		{"wrong content type", `{}`, "text/plain", 415, "UNSUPPORTED_MEDIA_TYPE", nil},
		{"missing content type", `{}`, "", 415, "UNSUPPORTED_MEDIA_TYPE", nil},
		{"broken json", `{`, "application/json", 400, "INVALID_JSON", nil},
		{"empty body", ``, "application/json", 400, "INVALID_JSON", nil},
		{"trailing data", `{"name":"a"}{"name":"b"}`, "application/json", 400, "INVALID_JSON", nil},
		{"unknown field", `{"name":"a","x":1}`, "application/json", 400, "VALIDATION_FAILED",
			map[string]any{"fields": []any{map[string]any{"field": "x", "reason": "unknown_field"}}}},
		{"wrong type", `{"name":5}`, "application/json", 400, "VALIDATION_FAILED",
			map[string]any{"fields": []any{map[string]any{"field": "name", "reason": "invalid_type"}}}},
		{"too large", `{"name":"` + strings.Repeat("a", 64*1024) + `"}`, "application/json", 413, "PAYLOAD_TOO_LARGE", nil},
		{"validate error", `{"name":"bad"}`, "application/json", 400, "VALIDATION_FAILED",
			map[string]any{"fields": []any{map[string]any{"field": "name", "reason": "not_allowed"}}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := do(h, http.MethodPost, c.body, c.ct)
			require.Equal(t, c.status, rec.Code)
			e := errorBody(t, rec)
			require.Equal(t, c.code, e["code"])
			require.Equal(t, testRequestID, e["request_id"])
			require.Equal(t, c.details, e["details"])
		})
	}
}

func TestHandleNoContent(t *testing.T) {
	h := httpapi.Handle(http.StatusNoContent, func(context.Context, httpapi.Request[createReq]) (httpapi.NoContent, error) {
		return httpapi.NoContent{}, nil
	})
	rec := do(h, http.MethodPost, `{"name":"a"}`, "application/json")
	require.Equal(t, http.StatusNoContent, rec.Code)
	require.Empty(t, rec.Body.String())
}

func TestHandleResponseEffectsOnSuccessAndError(t *testing.T) {
	cookie := &http.Cookie{Name: "kotoki_refresh", Value: "v", Path: "/api/v1/auth", HttpOnly: true}
	ok := httpapi.Handle(http.StatusCreated, func(_ context.Context, req httpapi.Request[createReq]) (createResp, error) {
		req.SetCookie(cookie)
		req.SetHeader("Cache-Control", "no-store")
		req.SetStatus(http.StatusOK)
		return createResp{Name: "n"}, nil
	})
	rec := do(ok, http.MethodPost, `{"name":"a"}`, "application/json")
	require.Equal(t, http.StatusOK, rec.Code, "SetStatus overrides the default status")
	require.Contains(t, rec.Header().Get("Set-Cookie"), "kotoki_refresh=v")
	require.Equal(t, "no-store", rec.Header().Get("Cache-Control"))

	fail := httpapi.Handle(http.StatusCreated, func(_ context.Context, req httpapi.Request[createReq]) (createResp, error) {
		req.SetCookie(cookie)
		req.SetHeader("X-Test", "1")
		return createResp{}, errs.New(errs.CodeRefreshTokenInvalid, "x")
	})
	rec = do(fail, http.MethodPost, `{"name":"a"}`, "application/json")
	require.Equal(t, http.StatusUnauthorized, rec.Code)
	require.Contains(t, rec.Header().Get("Set-Cookie"), "kotoki_refresh=v", "effects also apply to error responses")
	require.Equal(t, "1", rec.Header().Get("X-Test"))
}

func TestHandleNoBodyRequestFields(t *testing.T) {
	h := httpapi.HandleNoBody(http.StatusOK, func(_ context.Context, req httpapi.Request[struct{}]) (map[string]string, error) {
		return map[string]string{
			"rid": req.RequestID, "idem": req.IdempotencyKey, "ua": req.UserAgent, "ip": req.ClientIP.String(),
			"cookie": req.Cookie("kotoki_refresh"), "q": req.Query().Get("q"),
		}, nil
	})
	req := httptest.NewRequest(http.MethodGet, "/x?q=1", nil)
	req.RemoteAddr = "203.0.113.7:4444"
	req.Header.Set("Idempotency-Key", "k-1234567")
	req.Header.Set("User-Agent", "ua-test")
	req.AddCookie(&http.Cookie{Name: "kotoki_refresh", Value: "tok"})
	req = req.WithContext(httpapi.WithRequestID(req.Context(), testRequestID))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.JSONEq(t, `{"rid":"`+testRequestID+`","idem":"k-1234567","ua":"ua-test","ip":"203.0.113.7","cookie":"tok","q":"1"}`, rec.Body.String())
}

func TestPageEmpty(t *testing.T) {
	b, err := json.Marshal(httpapi.NewPage[string](nil, nil))
	require.NoError(t, err)
	require.JSONEq(t, `{"data":[],"next_cursor":null}`, string(b))
	next := "abc"
	b, _ = json.Marshal(httpapi.NewPage([]int{1}, &next))
	require.JSONEq(t, `{"data":[1],"next_cursor":"abc"}`, string(b))
}

func TestCursor(t *testing.T) {
	c := httpapi.EncodeCursor("0192f1a0-1111-7000-8000-000000000001")
	var got string
	require.NoError(t, httpapi.DecodeCursor(c, &got))
	require.Equal(t, "0192f1a0-1111-7000-8000-000000000001", got)

	arr := httpapi.EncodeCursor([]any{"MISC", "2026-10-14T09:00:00.000Z", "id1"})
	var gotArr []any
	require.NoError(t, httpapi.DecodeCursor(arr, &gotArr))
	require.Len(t, gotArr, 3)

	for _, bad := range []string{"!!!", "", base64.RawURLEncoding.EncodeToString([]byte(`{"v":2,"k":1}`)),
		base64.RawURLEncoding.EncodeToString([]byte(`not json`)), base64.RawURLEncoding.EncodeToString([]byte(`{"v":1,"k":1}`))} {
		var s string
		err := httpapi.DecodeCursor(bad, &s)
		require.Equal(t, errs.CodeInvalidCursor, errs.CodeOf(err), bad)
	}
}

func TestParseLimit(t *testing.T) {
	n, err := httpapi.ParseLimit(url.Values{})
	require.NoError(t, err)
	require.Equal(t, 20, n)
	n, err = httpapi.ParseLimit(url.Values{"limit": {"100"}})
	require.NoError(t, err)
	require.Equal(t, 100, n)
	for _, bad := range []string{"0", "101", "-1"} {
		_, err = httpapi.ParseLimit(url.Values{"limit": {bad}})
		require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err), bad)
		require.Equal(t, map[string]any{"fields": []map[string]any{{"field": "limit", "reason": "out_of_range"}}}, errs.DetailsOf(err))
	}
	_, err = httpapi.ParseLimit(url.Values{"limit": {"abc"}})
	require.Equal(t, map[string]any{"fields": []map[string]any{{"field": "limit", "reason": "invalid_type"}}}, errs.DetailsOf(err))
}
