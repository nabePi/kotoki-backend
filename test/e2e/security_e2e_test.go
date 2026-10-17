//go:build e2e

package e2e

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Automatic security checks of spec 13.8 (plan M10 Task 13) against the real cmd/api and
// cmd/realtime processes: response headers, CORS, CSRF, request ids, strict JSON, body limit,
// object-level authorization, WebSocket origin and ticket, error bodies, login rate limit.

// secResp is one raw HTTP exchange.
type secResp struct {
	Status int
	Header http.Header
	Body   []byte
}

// secDo sends one request to the API process. headers may set Authorization, Origin, Content-Type.
func secDo(t *testing.T, method, path string, headers map[string]string, body io.Reader) secResp {
	t.Helper()
	req, err := http.NewRequestWithContext(ctxT(t, 15*time.Second), method, stack.apiURL+path, body)
	require.NoError(t, err)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return secResp{Status: resp.StatusCode, Header: resp.Header, Body: raw}
}

func (r secResp) errorObject(t *testing.T) map[string]any {
	t.Helper()
	var env struct {
		Error map[string]any `json:"error"`
	}
	require.NoError(t, json.Unmarshal(r.Body, &env), string(r.Body))
	require.NotNil(t, env.Error, "D-013 error envelope: %s", r.Body)
	return env.Error
}

// clearLoginLimit empties the per-IP login bucket now and when the test ends, so the logins of the
// tests that follow (newUser) are not throttled by the ones made here.
func clearLoginLimit(t *testing.T) {
	t.Helper()
	clearRateLimit(t, "auth_login_ip")
	t.Cleanup(func() {
		keys, _ := stack.rdb.Keys(context.Background(), "kotoki:ratelimit:auth_login_ip:*").Result()
		if len(keys) > 0 {
			stack.rdb.Del(context.Background(), keys...)
		}
	})
}

func jsonHeaders(extra ...string) map[string]string {
	h := map[string]string{"Content-Type": "application/json"}
	for i := 0; i+1 < len(extra); i += 2 {
		h[extra[i]] = extra[i+1]
	}
	return h
}

func assertSecurityHeaders(t *testing.T, h http.Header, where string) {
	t.Helper()
	assert.Equal(t, "nosniff", h.Get("X-Content-Type-Options"), where)
	assert.Equal(t, "no-referrer", h.Get("Referrer-Policy"), where)
	assert.Equal(t, "DENY", h.Get("X-Frame-Options"), where)
	assert.Equal(t, "default-src 'none'; frame-ancestors 'none'", h.Get("Content-Security-Policy"), where)
}

func TestSecurity_ResponseHeaders(t *testing.T) {
	r := secDo(t, http.MethodGet, "/api/v1/auth/terms", nil, nil)
	require.Equal(t, http.StatusOK, r.Status)
	assertSecurityHeaders(t, r.Header, "GET /auth/terms")
	assert.Equal(t, "no-store", r.Header.Get("Cache-Control"), "every /auth/* response is uncacheable")

	// error paths carry the same headers
	assertSecurityHeaders(t, secDo(t, http.MethodGet, "/api/v1/no-such-route", nil, nil).Header, "404")
	assertSecurityHeaders(t, secDo(t, http.MethodGet, "/api/v1/me", nil, nil).Header, "401")

	u := newUser(t)
	login := secDo(t, http.MethodPost, "/api/v1/auth/login", jsonHeaders(),
		strings.NewReader(`{"email":"`+u.Email+`","password":"`+strongPassword+`"}`))
	require.Equal(t, http.StatusOK, login.Status, string(login.Body))
	assert.Equal(t, "no-store", login.Header.Get("Cache-Control"), "POST /auth/login")
	assertSecurityHeaders(t, login.Header, "POST /auth/login")

	me := secDo(t, http.MethodGet, "/api/v1/me", map[string]string{"Authorization": "Bearer " + u.Client.AccessToken()}, nil)
	require.Equal(t, http.StatusOK, me.Status)
	assert.Equal(t, "no-store", me.Header.Get("Cache-Control"), "GET /me")
	assertSecurityHeaders(t, me.Header, "GET /me")

	// the refresh cookie: not readable by scripts, never sent cross-site, scoped to /auth
	var refresh *http.Cookie
	for _, c := range (&http.Response{Header: login.Header}).Cookies() {
		if c.Value != "" {
			refresh = c
		}
	}
	require.NotNil(t, refresh, "login sets the refresh cookie")
	assert.True(t, refresh.HttpOnly)
	assert.Equal(t, http.SameSiteStrictMode, refresh.SameSite)
	assert.Equal(t, "/api/v1/auth", refresh.Path)
}

func TestSecurity_CORSExactMatch(t *testing.T) {
	r := secDo(t, http.MethodGet, "/api/v1/auth/terms", map[string]string{"Origin": testOrigin}, nil)
	require.Equal(t, http.StatusOK, r.Status)
	assert.Equal(t, testOrigin, r.Header.Get("Access-Control-Allow-Origin"))
	assert.Contains(t, r.Header.Values("Vary"), "Origin")
	assert.Equal(t, "true", r.Header.Get("Access-Control-Allow-Credentials"))

	pre := secDo(t, http.MethodOptions, "/api/v1/auth/login", map[string]string{
		"Origin": testOrigin, "Access-Control-Request-Method": "POST", "Access-Control-Request-Headers": "authorization,content-type",
	}, nil)
	assert.Equal(t, http.StatusNoContent, pre.Status)
	assert.Equal(t, testOrigin, pre.Header.Get("Access-Control-Allow-Origin"))
	assert.Equal(t, "600", pre.Header.Get("Access-Control-Max-Age"))
	assert.Equal(t, "GET, POST, PUT, PATCH, DELETE, OPTIONS", pre.Header.Get("Access-Control-Allow-Methods"))
	assert.Equal(t, "Authorization, Content-Type, Idempotency-Key, X-Request-ID", pre.Header.Get("Access-Control-Allow-Headers"))

	for _, origin := range []string{
		"http://localhost:3000.evil.test", "null", "http://localhost:3001", "https://localhost:3000",
		"http://LOCALHOST:3000", testOrigin + "/", "http://evil.test",
	} {
		for _, req := range []struct {
			method  string
			headers map[string]string
		}{
			{http.MethodGet, map[string]string{"Origin": origin}},
			{http.MethodOptions, map[string]string{"Origin": origin, "Access-Control-Request-Method": "POST"}},
		} {
			got := secDo(t, req.method, "/api/v1/auth/terms", req.headers, nil)
			for _, h := range []string{"Access-Control-Allow-Origin", "Access-Control-Allow-Credentials", "Access-Control-Allow-Methods", "Access-Control-Max-Age"} {
				assert.Empty(t, got.Header.Get(h), "%s %s with Origin %q must not get %s", req.method, "/auth/terms", origin, h)
			}
		}
	}
}

// TestSecurity_CSRFRefreshNeedsAllowedOrigin: a cross-site page cannot drive the cookie endpoints.
// Spec 13.8.2 asks for a missing Origin to be refused too, but spec Bab 3 (5.9.1) allows it for
// the CLI and server-to-server callers, who do not send one; the browser always does on a
// cross-site POST. This follows Bab 3 (deviation M10-T13).
func TestSecurity_CSRFRefreshNeedsAllowedOrigin(t *testing.T) {
	for _, path := range []string{"/api/v1/auth/refresh", "/api/v1/auth/logout"} {
		for _, origin := range []string{"http://evil.test", "null", "http://localhost:3000.evil.test", "https://localhost:3000"} {
			r := secDo(t, http.MethodPost, path, map[string]string{"Origin": origin, "Cookie": "kotoki_refresh=x"}, nil)
			assert.Equal(t, http.StatusForbidden, r.Status, "%s from %q", path, origin)
			assert.Equal(t, "ORIGIN_NOT_ALLOWED", r.errorObject(t)["code"], path)
		}
	}

	// the allowed origin passes the check (and then fails on the missing/invalid token, not on origin)
	r := secDo(t, http.MethodPost, "/api/v1/auth/refresh", map[string]string{"Origin": testOrigin}, nil)
	assert.NotEqual(t, http.StatusForbidden, r.Status)
	assert.Equal(t, http.StatusUnauthorized, r.Status, string(r.Body))
	// without Origin (CLI): checked by the token, not by origin
	r = secDo(t, http.MethodPost, "/api/v1/auth/refresh", nil, nil)
	assert.Equal(t, http.StatusUnauthorized, r.Status, string(r.Body))
	assert.NotEqual(t, "ORIGIN_NOT_ALLOWED", r.errorObject(t)["code"])
}

func TestSecurity_RequestID(t *testing.T) {
	good := uuid.NewString()
	r := secDo(t, http.MethodGet, "/api/v1/no-such-route", map[string]string{"X-Request-ID": good}, nil)
	assert.Equal(t, good, r.Header.Get("X-Request-ID"))
	assert.Equal(t, good, r.errorObject(t)["request_id"])

	for _, bad := range []string{"not-a-uuid", "12345", strings.Repeat("a", 200), "<script>alert(1)</script>", good + "x", "{" + good + "}"} {
		r := secDo(t, http.MethodGet, "/api/v1/no-such-route", map[string]string{"X-Request-ID": bad}, nil)
		got := r.Header.Get("X-Request-ID")
		parsed, err := uuid.Parse(got)
		require.NoError(t, err, "replacement for %q: %q", bad, got)
		assert.Equal(t, uuid.Version(7), parsed.Version(), "a replaced id is a UUIDv7")
		assert.Equal(t, got, r.errorObject(t)["request_id"])
	}
	// no header at all
	r = secDo(t, http.MethodGet, "/api/v1/no-such-route", nil, nil)
	parsed, err := uuid.Parse(r.Header.Get("X-Request-ID"))
	require.NoError(t, err)
	assert.Equal(t, uuid.Version(7), parsed.Version())
}

func TestSecurity_StrictJSONAndBodyLimit(t *testing.T) {
	clearLoginLimit(t)

	r := secDo(t, http.MethodPost, "/api/v1/auth/login", jsonHeaders(),
		strings.NewReader(`{"email":"a@example.com","password":"x","is_admin":true}`))
	assert.Equal(t, http.StatusBadRequest, r.Status, string(r.Body))
	assert.Equal(t, "VALIDATION_FAILED", r.errorObject(t)["code"], "an unknown field is a validation failure")

	// 70,000 bytes: over HTTP_MAX_BODY_BYTES (65,536), with a Content-Length and chunked
	big := `{"email":"a@example.com","password":"` + strings.Repeat("p", 70_000) + `"}`
	for name, body := range map[string]io.Reader{
		"content-length": strings.NewReader(big),
		"chunked":        io.NopCloser(strings.NewReader(big)),
	} {
		r = secDo(t, http.MethodPost, "/api/v1/auth/login", jsonHeaders(), body)
		assert.GreaterOrEqual(t, r.Status, 400, name)
		assert.Less(t, r.Status, 500, name)
		eo := r.errorObject(t)
		assert.Equal(t, "PAYLOAD_TOO_LARGE", eo["code"], name)
		assert.NotEmpty(t, eo["request_id"], name)
		assert.Less(t, len(r.Body), 1024, "the error does not echo the body")
	}

	// the body limit is not a ceiling the handlers skip: exactly under it is read normally
	ok := `{"email":"a@example.com","password":"` + strings.Repeat("p", 60_000) + `"}`
	r = secDo(t, http.MethodPost, "/api/v1/auth/login", jsonHeaders(), strings.NewReader(ok))
	assert.Less(t, r.Status, 500)
	assert.NotEqual(t, http.StatusRequestEntityTooLarge, r.Status)

	// wrong content type and trailing garbage are refused
	r = secDo(t, http.MethodPost, "/api/v1/auth/login", map[string]string{"Content-Type": "text/plain"}, strings.NewReader(`{}`))
	assert.Equal(t, http.StatusUnsupportedMediaType, r.Status)
	r = secDo(t, http.MethodPost, "/api/v1/auth/login", jsonHeaders(), strings.NewReader(`{"email":"a@example.com","password":"x"}{"a":1}`))
	assert.Equal(t, http.StatusBadRequest, r.Status)
}

func TestSecurity_HorizontalAuthz(t *testing.T) {
	a, b := newUser(t), newUser(t)
	s := startSession(t, a, "USER_FIRST")
	bAuth := map[string]string{"Authorization": "Bearer " + b.Client.AccessToken()}

	// the owner can; the baseline for every check below
	own := secDo(t, http.MethodGet, "/api/v1/sessions/"+s.ID.String(), map[string]string{"Authorization": "Bearer " + a.Client.AccessToken()}, nil)
	require.Equal(t, http.StatusOK, own.Status)

	for _, c := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/sessions/" + s.ID.String()},
		{http.MethodPost, "/api/v1/sessions/" + s.ID.String() + "/realtime-ticket"},
		{http.MethodGet, "/api/v1/sessions/" + s.ID.String() + "/evaluation"},
		{http.MethodGet, "/api/v1/sessions/" + s.ID.String() + "/turns"},
		{http.MethodPost, "/api/v1/sessions/" + s.ID.String() + "/cancel"},
	} {
		r := secDo(t, c.method, c.path, bAuth, nil)
		assert.Contains(t, []int{http.StatusForbidden, http.StatusNotFound}, r.Status, "%s %s: %s", c.method, c.path, r.Body)
		assert.NotContains(t, string(r.Body), a.ID.String(), "%s %s leaks the owner id", c.method, c.path)
		assert.NotContains(t, string(r.Body), a.Email, "%s %s leaks the owner email", c.method, c.path)
	}

	// B's cancel did nothing, and A's session is not in B's list
	assert.Equal(t, "RESERVED", readSession(t, s.ID).Status)
	list := secDo(t, http.MethodGet, "/api/v1/sessions", bAuth, nil)
	require.Equal(t, http.StatusOK, list.Status)
	assert.NotContains(t, string(list.Body), s.ID.String())

	// no ticket was issued to B for A's session
	keys, err := stack.rdb.Keys(t.Context(), "kotoki:ws_ticket:*").Result()
	require.NoError(t, err)
	for _, k := range keys {
		v, err := stack.rdb.Get(t.Context(), k).Result()
		if err != nil {
			continue
		}
		assert.False(t, strings.Contains(v, b.ID.String()) && strings.Contains(v, s.ID.String()), "ticket binding B to A's session")
	}

	// a forged or missing identity is not an identity
	r := secDo(t, http.MethodGet, "/api/v1/sessions/"+s.ID.String(), map[string]string{"Authorization": "Bearer " + a.Client.AccessToken() + "x"}, nil)
	assert.Equal(t, http.StatusUnauthorized, r.Status)
	r = secDo(t, http.MethodGet, "/api/v1/sessions/"+s.ID.String(), nil, nil)
	assert.Equal(t, http.StatusUnauthorized, r.Status)
	// a user token is not an admin token
	r = secDo(t, http.MethodGet, "/api/v1/admin/users", bAuth, nil)
	assert.Equal(t, http.StatusForbidden, r.Status)
}

func TestSecurity_WebSocketOriginAndQueryToken(t *testing.T) {
	u := newUser(t)
	s := startSession(t, u, "USER_FIRST")
	ticket, err := u.Client.RealtimeTicket(ctxT(t, 10*time.Second), s.ID)
	require.NoError(t, err)

	// a foreign Origin never reaches the upgrade, even with a valid ticket
	httpURL := "http://" + stack.rt.addr + "/api/v1/realtime?ticket=" + ticket
	for _, origin := range []string{"http://evil.test", "http://localhost:3000.evil.test", "null", "https://localhost:3000"} {
		req, err := http.NewRequestWithContext(ctxT(t, 10*time.Second), http.MethodGet, httpURL, http.NoBody)
		require.NoError(t, err)
		req.Header.Set("Origin", origin)
		req.Header.Set("Connection", "Upgrade")
		req.Header.Set("Upgrade", "websocket")
		req.Header.Set("Sec-WebSocket-Version", "13")
		req.Header.Set("Sec-WebSocket-Key", base64.StdEncoding.EncodeToString([]byte("0123456789abcdef")))
		req.Header.Set("Sec-WebSocket-Protocol", "kotoki.v1")
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		raw, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		assert.Equal(t, http.StatusForbidden, resp.StatusCode, "Origin %q: %s", origin, raw)
		assert.NotEqual(t, http.StatusSwitchingProtocols, resp.StatusCode)
		assert.Empty(t, resp.Header.Get("Sec-WebSocket-Accept"), "no upgrade for Origin %q", origin)
		var env struct{ Error struct{ Code string } }
		require.NoError(t, json.Unmarshal(raw, &env), string(raw))
		assert.Equal(t, "ORIGIN_NOT_ALLOWED", env.Error.Code)
	}
	// the refused handshakes did not spend the ticket
	n, err := stack.rdb.Exists(t.Context(), ticketKey(ticket)).Result()
	require.NoError(t, err)
	assert.EqualValues(t, 1, n, "an origin refusal happens before the ticket is consumed")

	// an access token (JWT) in the query is not a ticket: error event, close 4001, nothing bound
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, "ws://"+stack.rt.addr+"/api/v1/realtime?ticket="+u.Client.AccessToken(), &websocket.DialOptions{
		HTTPHeader: http.Header{"Origin": {testOrigin}}, Subprotocols: []string{"kotoki.v1"},
	})
	if err == nil {
		_, raw, rerr := c.Read(ctx)
		require.NoError(t, rerr)
		var ev struct{ Type string }
		require.NoError(t, json.Unmarshal(raw, &ev))
		assert.Equal(t, "error", ev.Type)
		_, _, rerr = c.Read(ctx)
		assert.Equal(t, websocket.StatusCode(4001), websocket.CloseStatus(rerr), "TICKET_INVALID")
		_ = c.CloseNow()
	}
	assert.Equal(t, "RESERVED", readSession(t, s.ID).Status, "no session was bound to the JWT")
}

func TestSecurity_ErrorBodiesLeakNothing(t *testing.T) {
	u := newUser(t)
	auth := map[string]string{"Authorization": "Bearer " + u.Client.AccessToken()}
	clearLoginLimit(t)

	forbidden := []string{"SQLSTATE", "pgx", "goroutine", ".go:", "panic", "runtime.", "sql:", "redis:", "connection refused", "/Users/", "/tmp/"}
	internalIP := regexp.MustCompile(`\b(10|127|172\.(1[6-9]|2\d|3[01])|192\.168)\.\d+\.\d+(\.\d+)?\b`)
	check := func(name string, r secResp) {
		t.Helper()
		body := string(r.Body)
		for _, f := range forbidden {
			assert.NotContains(t, body, f, "%s leaks %q: %s", name, f, body)
		}
		assert.False(t, internalIP.MatchString(body), "%s leaks an internal address: %s", name, body)
		assert.Equal(t, "application/json; charset=utf-8", r.Header.Get("Content-Type"), name)
		assert.NotEmpty(t, r.errorObject(t)["request_id"], name)
	}

	check("broken json", secDo(t, http.MethodPost, "/api/v1/auth/login", jsonHeaders(), strings.NewReader(`{"email":`)))
	check("not an object", secDo(t, http.MethodPost, "/api/v1/auth/login", jsonHeaders(), strings.NewReader(`[1,2,3]`)))
	check("wrong type", secDo(t, http.MethodPost, "/api/v1/auth/login", jsonHeaders(), strings.NewReader(`{"email":5,"password":{}}`)))
	check("unknown route", secDo(t, http.MethodGet, "/api/v1/definitely/not/here", nil, nil))
	check("wrong method", secDo(t, http.MethodDelete, "/api/v1/auth/terms", nil, nil))
	check("invalid uuid", secDo(t, http.MethodGet, "/api/v1/sessions/not-a-uuid", auth, nil))
	check("invalid uuid ticket", secDo(t, http.MethodPost, "/api/v1/sessions/%27%3BDROP%20TABLE%20users%3B--/realtime-ticket", auth, nil))
	check("sql in cursor", secDo(t, http.MethodGet, "/api/v1/sessions?cursor=%27%20OR%201%3D1--", auth, nil))
	check("sql in limit", secDo(t, http.MethodGet, "/api/v1/sessions?limit=1%3BDROP", auth, nil))
	check("bad token", secDo(t, http.MethodGet, "/api/v1/me", map[string]string{"Authorization": "Bearer a.b.c"}, nil))
	check("unknown session", secDo(t, http.MethodGet, "/api/v1/sessions/"+uuid.NewString(), auth, nil))
	check("unknown content", secDo(t, http.MethodPost, "/api/v1/sessions", jsonHeaders("Authorization", auth["Authorization"], "Idempotency-Key", uuid.NewString()),
		strings.NewReader(`{"topic_id":"`+uuid.NewString()+`","scenario_id":"`+uuid.NewString()+`","character_id":"`+uuid.NewString()+`","duration_seconds":300,"conversation_start_mode":"USER_FIRST"}`)))
}

func TestSecurity_LoginRateLimit(t *testing.T) {
	clearLoginLimit(t)
	before := scrape(t, stack.api.opsAddr)[`kotoki_ratelimit_rejected_total{policy="auth_login_ip"}`]

	// Twenty-five logins at once, each for a different unknown address, so only the per-IP policy
	// (20 per minute, refilling one token every 3 s) can trip and a slow runner cannot refill the
	// bucket between requests: 20 are answered (401), the rest are 429.
	const n = 25
	results := make(chan secResp, n)
	for range n {
		go func() {
			body := `{"email":"nobody-` + uuid.NewString() + `@example.com","password":"` + strongPassword + `"}`
			results <- secDo(t, http.MethodPost, "/api/v1/auth/login", jsonHeaders(), strings.NewReader(body))
		}()
	}
	var answered, limited []secResp
	for range n {
		r := <-results
		switch r.Status {
		case http.StatusUnauthorized:
			answered = append(answered, r)
		case http.StatusTooManyRequests:
			limited = append(limited, r)
		default:
			t.Fatalf("unexpected status %d: %s", r.Status, r.Body)
		}
	}
	assert.GreaterOrEqual(t, len(answered), 20)
	assert.LessOrEqual(t, len(answered), 21, "the bucket holds 20 (one refill at most)")
	require.NotEmpty(t, limited, "the 21st login of a minute is refused")
	for _, r := range limited {
		assert.Equal(t, "RATE_LIMITED", r.errorObject(t)["code"])
		assert.NotEmpty(t, r.Header.Get("Retry-After"))
		assert.Equal(t, "no-store", r.Header.Get("Cache-Control"))
	}

	after := scrape(t, stack.api.opsAddr)[`kotoki_ratelimit_rejected_total{policy="auth_login_ip"}`]
	assert.GreaterOrEqual(t, after-before, float64(len(limited)))
}
