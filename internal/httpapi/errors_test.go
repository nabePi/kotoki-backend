package httpapi_test

import (
	"bytes"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/httpapi"
	"github.com/nabePi/kotoki-backend/internal/platform/errs"
	"github.com/nabePi/kotoki-backend/internal/testutil"
)

var update = flag.Bool("update", false, "regenerate testdata/api/rest/errors/*.json")

// wantStatus is the HTTP column of spec 5.10, copied row by row (50 REST codes).
var wantStatus = map[string]int{
	"INVALID_JSON":                   400,
	"VALIDATION_FAILED":              400,
	"INVALID_CURSOR":                 400,
	"IDEMPOTENCY_KEY_REQUIRED":       400,
	"IDEMPOTENCY_KEY_INVALID":        400,
	"PASSWORD_TOO_WEAK":              400,
	"RESET_TOKEN_INVALID":            400,
	"AUTH_REQUIRED":                  401,
	"TOKEN_INVALID":                  401,
	"TOKEN_EXPIRED":                  401,
	"INVALID_CREDENTIALS":            401,
	"REFRESH_TOKEN_INVALID":          401,
	"BALANCE_INSUFFICIENT":           402,
	"FORBIDDEN":                      403,
	"ACCOUNT_SUSPENDED":              403,
	"TERMS_NOT_ACCEPTED":             403,
	"ORIGIN_NOT_ALLOWED":             403,
	"NOT_FOUND":                      404,
	"USER_NOT_FOUND":                 404,
	"SESSION_NOT_FOUND":              404,
	"EVALUATION_NOT_AVAILABLE":       404,
	"MEMORY_NOT_FOUND":               404,
	"METHOD_NOT_ALLOWED":             405,
	"EMAIL_ALREADY_REGISTERED":       409,
	"PHONE_ALREADY_REGISTERED":       409,
	"REFRESH_TOKEN_ROTATED":          409,
	"IDEMPOTENCY_KEY_REUSED":         409,
	"SESSION_ALREADY_OPEN":           409,
	"SESSION_NOT_CANCELLABLE":        409,
	"SESSION_NOT_CONNECTABLE":        409,
	"SESSION_START_DEADLINE_PASSED":  409,
	"SESSION_NOT_ENDED":              409,
	"ACCOUNT_HAS_ACTIVE_SESSION":     409,
	"BALANCE_ADJUSTMENT_NEGATIVE":    409,
	"CANNOT_MODIFY_SELF":             409,
	"INVALID_USER_STATUS_TRANSITION": 409,
	"CONTENT_INVALID_TRANSITION":     409,
	"CONTENT_VERSION_REQUIRED":       409,
	"TRANSCRIPT_EXPIRED":             410,
	"PAYLOAD_TOO_LARGE":              413,
	"UNSUPPORTED_MEDIA_TYPE":         415,
	"TOPIC_NOT_AVAILABLE":            422,
	"SCENARIO_NOT_AVAILABLE":         422,
	"SCENARIO_TOPIC_MISMATCH":        422,
	"CHARACTER_NOT_AVAILABLE":        422,
	"CONTENT_VALIDATION_FAILED":      422,
	"RATE_LIMITED":                   429,
	"INTERNAL_ERROR":                 500,
	"CONTENT_MISCONFIGURED":          500,
	"SERVICE_UNAVAILABLE":            503}

func TestStatusOf(t *testing.T) {
	require.Equal(t, 402, httpapi.StatusOf(errs.CodeBalanceInsufficient))
	require.Equal(t, 429, httpapi.StatusOf(errs.CodeRateLimited))
	require.Equal(t, 500, httpapi.StatusOf(errs.CodeInternalError))
	require.Equal(t, 500, httpapi.StatusOf(errs.Code("NO_SUCH_CODE")))
	for code, want := range wantStatus {
		require.Equal(t, want, httpapi.StatusOf(errs.Code(code)), code)
	}
}

func TestRestCatalogHas50Codes(t *testing.T) {
	require.Len(t, wantStatus, 50)
	for code := range wantStatus {
		_, ok := errs.Messages[errs.Code(code)]
		require.True(t, ok, "%s must have a message", code)
	}
}

func writeErr(err error) (*httptest.ResponseRecorder, string) {
	logs := &bytes.Buffer{}
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	ctx := httpapi.WithRequestID(req.Context(), testRequestID)
	ctx = httpapi.WithLogger(ctx, slog.New(slog.NewJSONHandler(logs, nil)))
	rec := httptest.NewRecorder()
	httpapi.WriteError(rec, req.WithContext(ctx), err)
	return rec, logs.String()
}

func TestWriteErrorGolden(t *testing.T) {
	dir := filepath.Join(testutil.ModuleRoot(t), "testdata", "api", "rest", "errors")
	codes := make([]string, 0, len(wantStatus))
	for c := range wantStatus {
		codes = append(codes, c)
	}
	sort.Strings(codes)
	for _, code := range codes {
		t.Run(code, func(t *testing.T) {
			rec, _ := writeErr(errs.New(errs.Code(code), "internal text that must never leak"))
			require.Equal(t, wantStatus[code], rec.Code)
			require.Equal(t, "application/json; charset=utf-8", rec.Header().Get("Content-Type"))
			golden := filepath.Join(dir, code+".json")
			if *update {
				require.NoError(t, os.MkdirAll(dir, 0o755))
				require.NoError(t, os.WriteFile(golden, rec.Body.Bytes(), 0o644))
				return
			}
			want, err := os.ReadFile(golden)
			require.NoError(t, err, "run: go test ./internal/httpapi -run WriteErrorGolden -update")
			require.Equal(t, string(want), rec.Body.String())
			require.Contains(t, rec.Body.String(), `"request_id":"`+testRequestID+`"`)
			require.Contains(t, rec.Body.String(), `"details":null`)
			require.NotContains(t, rec.Body.String(), "must never leak")
		})
	}
}

func TestWriteErrorUnknownAndWSOnlyCodes(t *testing.T) {
	internal := errs.Messages[errs.CodeInternalError]
	var wsOnly []errs.Code
	for _, c := range errs.AllCodes() {
		if _, rest := wantStatus[string(c)]; rest {
			continue
		}
		// pre-upgrade HTTP rejections of the realtime gateway (spec 7.3.1 step 3, M5)
		if c == errs.CodeServerDraining || c == errs.CodeServerAtCapacity {
			require.Equal(t, 503, httpapi.StatusOf(c), c)
			continue
		}
		wsOnly = append(wsOnly, c)
	}
	require.Len(t, wsOnly, 15)
	for _, c := range append(wsOnly, errs.Code("NO_SUCH_CODE")) {
		rec, logs := writeErr(errs.WithDetails(errs.New(c, "x"), map[string]any{"secret": "leak"}))
		require.Equal(t, 500, rec.Code, c)
		e := errorBody(t, rec)
		require.Equal(t, "INTERNAL_ERROR", e["code"], c)
		require.Equal(t, internal, e["message"], c)
		require.Nil(t, e["details"], "details of an unknown code must not leak")
		require.Contains(t, logs, `"level":"ERROR"`)
	}
	rec, _ := writeErr(errors.New("plain error"))
	require.Equal(t, 500, rec.Code)
	require.Equal(t, "INTERNAL_ERROR", errorBody(t, rec)["code"])
}

func TestWriteErrorDetailsAndHeaders(t *testing.T) {
	rec, _ := writeErr(errs.WithDetails(errs.New(errs.CodeBalanceInsufficient, "x"),
		map[string]any{"available_seconds": 600, "requested_seconds": 900}))
	require.Equal(t, 402, rec.Code)
	require.Equal(t, map[string]any{"available_seconds": float64(600), "requested_seconds": float64(900)}, errorBody(t, rec)["details"])

	rec, _ = writeErr(errs.New(errs.CodeServiceUnavailable, "x"))
	require.Equal(t, 503, rec.Code)
	require.Equal(t, "1", rec.Header().Get("Retry-After"))

	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	rec = httptest.NewRecorder()
	rec.Header().Set("Retry-After", "2")
	httpapi.WriteError(rec, req, errs.New(errs.CodeServiceUnavailable, "x"))
	require.Equal(t, "2", rec.Header().Get("Retry-After"), "a handler-chosen Retry-After wins")
}

func TestWriteErrorLogsInternalErrorsOnce(t *testing.T) {
	_, logs := writeErr(errs.New(errs.CodeInternalError, "boom"))
	require.Equal(t, 1, bytes.Count([]byte(logs), []byte(`"level":"ERROR"`)))
	require.Contains(t, logs, `"error_code":"INTERNAL_ERROR"`)
	require.Contains(t, logs, `"error_location"`)

	_, logs = writeErr(errs.New(errs.CodeBalanceInsufficient, "business"))
	require.NotContains(t, logs, `"level":"ERROR"`, "business errors are not logged as ERROR")
}

func TestWriteErrorGeneratesRequestID(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	rec := httptest.NewRecorder()
	httpapi.WriteError(rec, req, errs.New(errs.CodeNotFound, "x"))
	require.Regexp(t, `^[0-9a-f-]{36}$`, errorBody(t, rec)["request_id"])
}
