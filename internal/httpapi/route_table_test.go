package httpapi_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// routeRow is one line of the endpoint index (spec 5.8.5). Ready marks the rows
// the current milestone implements; the others document what later milestones add.
type routeRow struct {
	Method, Path, Group string
	Policies            []string
	Ready               bool
	Body                string // minimal valid body used to reach the handler
}

const (
	grpPublic = "public"
	grpUser   = "user"
	grpAdmin  = "admin"
)

var loginBody = `{"email":"hana@example.com","password":"correct horse battery"}`

var routeTable = []routeRow{
	{"POST", "/auth/register", grpPublic, []string{"auth_register_ip"}, true, `{"email":"new@example.com","password":"correct horse battery","full_name":"Hana","terms_version":"` + apiTerms + `"}`},
	{"POST", "/auth/login", grpPublic, []string{"auth_login_ip", "auth_login_email"}, true, loginBody},
	{"POST", "/auth/refresh", grpPublic, []string{"auth_refresh_ip"}, true, ""},
	{"POST", "/auth/logout", grpPublic, []string{"auth_refresh_ip"}, true, ""},
	{"POST", "/auth/forgot-password", grpPublic, []string{"auth_forgot_ip", "auth_forgot_email"}, true, `{"email":"hana@example.com"}`},
	{"POST", "/auth/reset-password", grpPublic, []string{"auth_reset_ip"}, true, `{"token":"short","new_password":"another long passphrase"}`},
	{"GET", "/auth/terms", grpPublic, []string{"api_ip_default"}, true, ""},
	{"GET", "/me", grpUser, []string{"api_user_default"}, true, ""},
	{"PUT", "/me/consents", grpUser, []string{"api_user_default"}, true, `{"terms_version":"` + apiTerms + `"}`},
	{"DELETE", "/me", grpUser, []string{"account_delete_user"}, true, `{"password":"wrong password!!"}`},

	{"GET", "/topics", grpUser, []string{"api_user_default"}, true, ""},
	{"GET", "/scenarios", grpUser, []string{"api_user_default"}, true, ""},
	{"GET", "/scenarios/{scenario_id}", grpUser, []string{"api_user_default"}, true, ""},
	{"GET", "/characters", grpUser, []string{"api_user_default"}, true, ""},
	{"GET", "/characters/{character_id}", grpUser, []string{"api_user_default"}, true, ""},
	{"POST", "/sessions", grpUser, []string{"session_create_user"}, false, ""},
	{"GET", "/sessions", grpUser, []string{"api_user_default"}, false, ""},
	{"GET", "/sessions/{session_id}", grpUser, []string{"api_user_default"}, false, ""},
	{"POST", "/sessions/{session_id}/cancel", grpUser, []string{"api_user_default"}, false, ""},
	{"POST", "/sessions/{session_id}/realtime-ticket", grpUser, []string{"ticket_user"}, false, ""},
	{"GET", "/sessions/{session_id}/turns", grpUser, []string{"api_user_default"}, false, ""},
	{"GET", "/sessions/{session_id}/evaluation", grpUser, []string{"api_user_default"}, false, ""},
	{"GET", "/balance", grpUser, []string{"api_user_default"}, true, ""},
	{"GET", "/balance/transactions", grpUser, []string{"api_user_default"}, true, ""},
	{"GET", "/profile", grpUser, []string{"api_user_default"}, false, ""},
	{"PATCH", "/profile", grpUser, []string{"api_user_default"}, false, ""},
	{"GET", "/profile/skills", grpUser, []string{"api_user_default"}, false, ""},
	{"GET", "/profile/skills/{skill}/history", grpUser, []string{"api_user_default"}, false, ""},
	{"DELETE", "/profile/skills", grpUser, []string{"api_user_default"}, false, ""},
	{"GET", "/profile/memories", grpUser, []string{"api_user_default"}, false, ""},
	{"DELETE", "/profile/memories", grpUser, []string{"api_user_default"}, false, ""},
	{"DELETE", "/profile/memories/{memory_id}", grpUser, []string{"api_user_default"}, false, ""},
	{"GET", "/admin/users", grpAdmin, []string{"admin_user"}, true, ""},
	{"GET", "/admin/users/{user_id}", grpAdmin, []string{"admin_user"}, true, ""},
	{"PATCH", "/admin/users/{user_id}", grpAdmin, []string{"admin_user"}, true, ""},
	{"POST", "/admin/users/{user_id}/balance-grants", grpAdmin, []string{"admin_user"}, true, ""},
	{"POST", "/admin/users/{user_id}/balance-adjustments", grpAdmin, []string{"admin_user"}, true, ""},
	{"GET", "/admin/users/{user_id}/balance/transactions", grpAdmin, []string{"admin_user"}, true, ""},
	{"GET", "/admin/users/{user_id}/sessions", grpAdmin, []string{"admin_user"}, false, ""},
	{"GET", "/admin/sessions/{session_id}", grpAdmin, []string{"admin_user"}, false, ""},
	{"GET", "/admin/sessions/{session_id}/turns", grpAdmin, []string{"admin_user"}, false, ""},
	{"GET", "/admin/sessions/{session_id}/events", grpAdmin, []string{"admin_user"}, false, ""},
	{"GET", "/admin/audit-logs", grpAdmin, []string{"admin_user"}, true, ""},
	{"GET", "/admin/content/validation-report", grpAdmin, []string{"admin_user"}, true, ""},
	{"GET", "/admin/content/characters/{id}", grpAdmin, []string{"admin_user"}, true, ""},
	{"PATCH", "/admin/content/characters/{id}", grpAdmin, []string{"admin_user"}, true, `{}`},
	{"GET", "/admin/content/{family}/{parent}/versions", grpAdmin, []string{"admin_user"}, true, ""},
	{"POST", "/admin/content/{family}/{parent}/versions", grpAdmin, []string{"admin_user"}, true, `{}`},
	{"GET", "/admin/content/{family}/{parent}/versions/{version_id}", grpAdmin, []string{"admin_user"}, true, ""},
	{"POST", "/admin/content/{family}/{parent}/versions/{version_id}/publish", grpAdmin, []string{"admin_user"}, true, ""},
	{"POST", "/admin/content/{family}/{parent}/versions/{version_id}/retire", grpAdmin, []string{"admin_user"}, true, ""},
	{"DELETE", "/admin/content/{family}/{parent}/versions/{version_id}", grpAdmin, []string{"admin_user"}, true, ""},
	{"PATCH", "/admin/content/{entity}/{id}/status", grpAdmin, []string{"admin_user"}, true, `{"status":"DISABLED"}`},
}

func concrete(path string) string {
	out := path
	for strings.Contains(out, "{") {
		i, j := strings.Index(out, "{"), strings.Index(out, "}")
		out = out[:i] + uuid.NewString() + out[j+1:]
	}
	return "/api/v1" + out
}

func TestRouteTable(t *testing.T) {
	e := newAPI(t)
	hana := e.register(t, "hana@example.com")
	userAuthz := bearerOf(t, hana)

	// 1. the set of registered routes equals the Ready rows
	var got, want []string
	require.NoError(t, chi.Walk(e.handler.(chi.Routes), func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		got = append(got, method+" "+route)
		return nil
	}))
	for _, r := range routeTable {
		if r.Ready {
			want = append(want, r.Method+" /api/v1"+r.Path)
		}
	}
	sort.Strings(got)
	sort.Strings(want)
	require.Equal(t, want, got)

	// 2. group and policies are properties of behavior, not of declarations
	for _, r := range routeTable {
		if !r.Ready {
			continue
		}
		t.Run(r.Method+" "+r.Path, func(t *testing.T) {
			path := concrete(r.Path)

			rec := e.do(r.Method, path, r.Body)
			code := codeOf(t, rec)
			switch r.Group {
			case grpPublic:
				require.NotEqual(t, "AUTH_REQUIRED", code, "public routes need no Authorization")
			default:
				require.Equal(t, "AUTH_REQUIRED", code)
			}

			if r.Group == grpAdmin {
				rec := e.do(r.Method, path, r.Body, "Authorization", userAuthz)
				require.Equal(t, "FORBIDDEN", codeOf(t, rec), "a USER token is refused on admin routes")
			}

			e.limiter.reset()
			headers := []string{}
			if r.Group != grpPublic {
				headers = append(headers, "Authorization", userAuthz)
			}
			e.do(r.Method, path, r.Body, headers...)
			require.Equal(t, r.Policies, e.limiter.called())
		})
	}
}

func TestInternalPathsNotServed(t *testing.T) {
	e := newAPI(t)
	for _, p := range []string{"/healthz", "/readyz", "/metrics", "/debug/pprof/", "/internal/providers"} {
		rec := e.do(http.MethodGet, p, "")
		require.Equal(t, 404, rec.Code, p)
		require.Equal(t, "NOT_FOUND", errorBody(t, rec)["code"], p)
	}
}

func TestMethodNotAllowed(t *testing.T) {
	e := newAPI(t)
	rec := e.do(http.MethodDelete, "/api/v1/auth/terms", "")
	require.Equal(t, 405, rec.Code)
	require.Equal(t, "METHOD_NOT_ALLOWED", errorBody(t, rec)["code"])
	rec = e.do(http.MethodGet, "/api/v1/nope", "")
	require.Equal(t, 404, rec.Code)
}

// codeOf returns the error code of a response, or "" when the body is empty or not an error.
func codeOf(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	if rec.Body.Len() == 0 {
		return ""
	}
	var out struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return out.Error.Code
}
