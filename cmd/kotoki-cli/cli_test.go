package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"flag"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/conversation/protocol"
	"github.com/nabePi/kotoki-backend/internal/kotokiclient"
	"github.com/nabePi/kotoki-backend/internal/kotokiclient/audio"
)

var (
	testSession   = uuid.MustParse("01929a40-1111-7abc-8def-000000000001")
	testTopic     = uuid.MustParse("01929a40-2222-7abc-8def-000000000001")
	testScenario  = uuid.MustParse("01929a40-3333-7abc-8def-000000000001")
	testScenario2 = uuid.MustParse("01929a40-3333-7abc-8def-000000000002")
	testCharacter = uuid.MustParse("01929a40-4444-7abc-8def-000000000001")
	testChar2     = uuid.MustParse("01929a40-4444-7abc-8def-000000000002")
)

// backend is a fake of the API and the realtime server in one httptest server.
type backend struct {
	t    *testing.T
	name string
	srv  *httptest.Server

	mu          sync.Mutex
	hits        map[string]int
	registerReq map[string]any
	createReq   map[string]any
	createKey   string
	auth        map[string][]string // path -> Authorization headers seen
	startMode   string              // USER_FIRST, AI_FIRST, or RESUME (the fake asks for session.resume)
	validAccess string              // when set, other access tokens get 401 TOKEN_EXPIRED
	validRefr   string
	evalStatus  []string // statuses served in order (the last one repeats)
	wsConns     []*wsRecord
	turnQueries []string
	turn        int // turns and generations are numbered across connections
}

type wsRecord struct {
	ticket string
	origin string
	events []protocol.ClientEvent
	frames int
}

func newBackend(t *testing.T, name string) *backend {
	t.Helper()
	b := &backend{t: t, name: name, hits: map[string]int{}, auth: map[string][]string{}, startMode: "USER_FIRST"}
	mux := http.NewServeMux()
	handle := func(pattern string, h http.HandlerFunc) {
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			b.mu.Lock()
			b.hits[r.Method+" "+r.URL.Path]++
			b.auth[r.URL.Path] = append(b.auth[r.URL.Path], r.Header.Get("Authorization"))
			stale := b.validAccess != "" && !strings.HasPrefix(r.URL.Path, "/api/v1/auth/") &&
				r.Header.Get("Authorization") != "Bearer "+b.validAccess
			b.mu.Unlock()
			if stale {
				apiErr(w, 401, "TOKEN_EXPIRED")
				return
			}
			h(w, r)
		})
	}
	handle("GET /api/v1/auth/terms", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, 200, map[string]any{"terms_version": "2026-10"})
	})
	handle("POST /api/v1/auth/register", func(w http.ResponseWriter, r *http.Request) {
		var in map[string]any
		_ = json.NewDecoder(r.Body).Decode(&in)
		b.mu.Lock()
		b.registerReq = in
		b.mu.Unlock()
		setRefresh(w, "refresh-"+name)
		writeJSON(w, 201, authBody("tok-"+name))
	})
	handle("POST /api/v1/auth/login", func(w http.ResponseWriter, _ *http.Request) {
		setRefresh(w, "refresh-"+name)
		writeJSON(w, 200, authBody("tok-"+name))
	})
	handle("POST /api/v1/auth/refresh", func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie("kotoki_refresh")
		b.mu.Lock()
		ok := err == nil && c.Value == b.validRefr
		b.mu.Unlock()
		if !ok {
			apiErr(w, 401, "REFRESH_TOKEN_INVALID")
			return
		}
		setRefresh(w, "r-rotated")
		writeJSON(w, 200, map[string]any{"access_token": "tok-refreshed"})
	})
	handle("GET /api/v1/topics", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, 200, map[string]any{"items": []map[string]any{{"id": testTopic, "code": "cafe", "name": "Cafe"}}})
	})
	handle("GET /api/v1/scenarios", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, 200, map[string]any{"items": []map[string]any{
			{"id": testScenario, "topic_id": testTopic, "code": "order_coffee", "name": "Order coffee"},
			{"id": testScenario2, "topic_id": testTopic, "code": "ask_way", "name": "Ask the way"},
		}})
	})
	handle("GET /api/v1/characters", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, 200, map[string]any{"items": []map[string]any{
			{"id": testCharacter, "code": "sakura", "name": "Sakura"}, {"id": testChar2, "code": "ken", "name": "Ken"},
		}})
	})
	handle("POST /api/v1/sessions", func(w http.ResponseWriter, r *http.Request) {
		var in map[string]any
		_ = json.NewDecoder(r.Body).Decode(&in)
		b.mu.Lock()
		b.createReq, b.createKey = in, r.Header.Get("Idempotency-Key")
		b.startMode, _ = in["conversation_start_mode"].(string)
		b.mu.Unlock()
		writeJSON(w, 201, map[string]any{"id": testSession, "status": "RESERVED", "duration_seconds": in["duration_seconds"],
			"conversation_start_mode": in["conversation_start_mode"]})
	})
	handle("GET /api/v1/sessions/{id}", func(w http.ResponseWriter, _ *http.Request) {
		b.mu.Lock()
		mode := b.startMode
		b.mu.Unlock()
		writeJSON(w, 200, map[string]any{"id": testSession, "status": "RESERVED", "duration_seconds": 300, "conversation_start_mode": mode})
	})
	handle("POST /api/v1/sessions/{id}/cancel", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, 200, map[string]any{"id": testSession, "status": "CANCELLED"})
	})
	handle("POST /api/v1/sessions/{id}/realtime-ticket", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, 200, map[string]any{"session_id": testSession, "ticket": "tkt-" + name, "expires_in": 30})
	})
	handle("GET /api/v1/balance", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, 200, map[string]any{"available_seconds": 3600, "reserved_seconds": 0})
	})
	handle("GET /api/v1/sessions/{id}/turns", func(w http.ResponseWriter, r *http.Request) {
		b.mu.Lock()
		b.turnQueries = append(b.turnQueries, r.URL.RawQuery)
		b.mu.Unlock()
		if r.URL.Query().Get("cursor") == "" {
			writeJSON(w, 200, map[string]any{"data": []map[string]any{{"sequence_number": 1, "status": "COMPLETED", "user_text": "はじめまして", "assistant_text": "こんにちは"}}, "next_cursor": "c1"})
			return
		}
		writeJSON(w, 200, map[string]any{"data": []map[string]any{{"sequence_number": 2, "status": "COMPLETED", "user_text": "元気です"}}, "next_cursor": nil})
	})
	handle("GET /api/v1/sessions/{id}/evaluation", func(w http.ResponseWriter, _ *http.Request) {
		b.mu.Lock()
		st := "COMPLETED"
		if len(b.evalStatus) > 0 {
			st = b.evalStatus[0]
			if len(b.evalStatus) > 1 {
				b.evalStatus = b.evalStatus[1:]
			}
		}
		b.mu.Unlock()
		writeJSON(w, 200, map[string]any{"status": st, "overall_score": 80})
	})
	mux.HandleFunc("GET /api/v1/realtime", b.serveWS)
	b.srv = httptest.NewServer(mux)
	t.Cleanup(b.srv.Close)
	return b
}

func setRefresh(w http.ResponseWriter, v string) {
	http.SetCookie(w, &http.Cookie{Name: "kotoki_refresh", Value: v, Path: "/api/v1/auth", HttpOnly: true, MaxAge: 3600})
}

func (b *backend) wsURL() string {
	return "ws" + strings.TrimPrefix(b.srv.URL, "http") + "/api/v1/realtime"
}

func (b *backend) hitCount(k string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.hits[k]
}

func (b *backend) totalHits() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := 0
	for _, v := range b.hits {
		n += v
	}
	return n
}

func (b *backend) conns() []*wsRecord {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]*wsRecord(nil), b.wsConns...)
}

func (b *backend) nextTurn() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.turn++
	return b.turn
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func apiErr(w http.ResponseWriter, status int, code string) {
	writeJSON(w, status, map[string]any{"error": map[string]any{"code": code, "message": "nope", "request_id": "rq"}})
}

func authBody(token string) map[string]any {
	return map[string]any{"access_token": token, "user": map[string]any{"id": "user-1", "email": "dev@example.test"}}
}

// ---- realtime part ----------------------------------------------------------------------

const quietFrames = 25 // the fake server ends a turn after this many silent frames (500 ms)

var silence = audio.SilencePacket()

func (b *backend) serveWS(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{"kotoki.v1"}, InsecureSkipVerify: true})
	if err != nil {
		return
	}
	defer func() { _ = conn.CloseNow() }()
	rec := &wsRecord{ticket: r.URL.Query().Get("ticket"), origin: r.Header.Get("Origin")}
	b.mu.Lock()
	b.wsConns = append(b.wsConns, rec)
	idx := len(b.wsConns)
	mode := b.startMode
	b.mu.Unlock()
	ctx := r.Context()

	var seq int64
	send := func(typ string, fixed bool, turn int, gen uint32, payload any) {
		ev := protocol.ServerEvent{Type: typ, EventID: uuid.New(), SessionID: testSession, TS: time.Now(), Payload: payload}
		if !fixed {
			seq++
			ev.ServerSeq = seq
		}
		if turn > 0 {
			ev.TurnSeq = &turn
		}
		if gen > 0 {
			ev.Generation = &gen
		}
		raw, err := protocol.MarshalServer(ev)
		if err != nil {
			panic(err)
		}
		_ = conn.Write(ctx, websocket.MessageText, raw)
	}
	answer := func(turn int, withUser bool) {
		gen := uint32(turn)
		if withUser {
			send("turn.started", false, turn, 0, protocol.TurnStarted{Initiator: "USER"})
			send("stt.final", false, turn, 0, protocol.STTFinal{Text: "こんにちは", EndReason: "SILENCE"})
		}
		send("ai.generation_started", false, turn, gen, protocol.AIGenerationStarted{Kind: "REPLY"})
		for i := range 3 {
			_ = conn.Write(ctx, websocket.MessageBinary, protocol.EncodeDownstream(gen, uint32(i), silence))
		}
		send("ai.audio_completed", false, turn, gen, protocol.AIAudioCompleted{FrameCount: 3, TotalAudioMS: 60, SentenceCount: 1, LLMFinished: true})
		send("turn.completed", false, turn, 0, protocol.TurnCompleted{Status: "COMPLETED",
			Latency: &protocol.Latency{TurnEndToFirstAudioMS: 800, STTFinalMS: 100, LLMFirstTokenMS: 400, TTSFirstAudioMS: 200}})
	}

	expect := "session.start"
	if mode == "RESUME" || idx > 1 {
		expect = "session.resume"
	}
	send("session.ready", true, 0, 0, protocol.SessionReady{Status: "RESERVED", ExpectedCommand: expect, ProtocolVersion: 1})
	var speaking bool
	var quiet int
	started := false
	for {
		typ, data, err := conn.Read(ctx)
		if err != nil {
			return
		}
		if typ == websocket.MessageText {
			ev, derr := protocol.DecodeClientEvent(data)
			if derr != nil {
				continue
			}
			b.mu.Lock()
			rec.events = append(rec.events, ev)
			b.mu.Unlock()
			switch ev.Type {
			case "session.start":
				send("session.started", false, 0, 0, protocol.SessionStarted{Status: "ACTIVE", TurnState: "LISTENING"})
				started = true
				if mode == "AI_FIRST" {
					answer(b.nextTurn(), false)
				}
			case "session.resume":
				send("session.snapshot", false, 0, 0, protocol.SessionSnapshot{Replay: "none", TurnState: "LISTENING"})
				started = true
			case "session.end":
				send("session.completed", false, 0, 0, protocol.SessionCompleted{EndReason: "USER_ENDED", ChargedSeconds: 7})
			}
			continue
		}
		_, opus, derr := protocol.DecodeUpstream(data)
		if derr != nil || !started {
			continue
		}
		b.mu.Lock()
		rec.frames++
		b.mu.Unlock()
		if !bytes.Equal(opus, silence) {
			speaking, quiet = true, 0
			continue
		}
		if speaking {
			quiet++
			if quiet == quietFrames {
				speaking, quiet = false, 0
				answer(b.nextTurn(), true)
			}
		}
	}
}

// ---- helpers ----------------------------------------------------------------------------

// cli runs the command line against the backend with a private config directory.
type cli struct {
	t   *testing.T
	xdg string
	env map[string]string
}

func newCLI(t *testing.T, b *backend) *cli {
	t.Helper()
	c := &cli{t: t, xdg: t.TempDir(), env: map[string]string{}}
	c.env["XDG_CONFIG_HOME"] = c.xdg
	if b != nil {
		c.env["KOTOKI_CLI_API_URL"] = b.srv.URL
		c.env["KOTOKI_CLI_WS_URL"] = b.wsURL()
		c.env["KOTOKI_CLI_ORIGIN"] = "http://app.test"
	}
	return c
}

func (c *cli) run(args ...string) (code int, stdout, stderr string) {
	c.t.Helper()
	var out, errw bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	code = run(ctx, args, &out, &errw, func(k string) string { return c.env[k] })
	return code, out.String(), errw.String()
}

func (c *cli) mustRun(args ...string) string {
	c.t.Helper()
	code, out, errw := c.run(args...)
	require.Equal(c.t, 0, code, "args %v\nstdout: %s\nstderr: %s", args, out, errw)
	return out
}

func (c *cli) credPath() string { return filepath.Join(c.xdg, "kotoki-cli", "credentials.json") }

func (c *cli) creds() credFile {
	c.t.Helper()
	raw, err := os.ReadFile(c.credPath())
	require.NoError(c.t, err)
	var f credFile
	require.NoError(c.t, json.Unmarshal(raw, &f))
	return f
}

func (c *cli) store() credStore {
	s, err := newCredStore(func(k string) string { return c.env[k] })
	require.NoError(c.t, err)
	return s
}

func (c *cli) login() {
	c.t.Helper()
	c.mustRun("login", "--email", "dev@example.test", "--password", "pw")
}

// speechWAV writes a mono 48 kHz sine of d to a temp file.
func speechWAV(t *testing.T, d time.Duration) string {
	t.Helper()
	n := int(d.Seconds() * 48000)
	s := make([]int16, n)
	for i := range s {
		s[i] = int16(8000 * math.Sin(2*math.Pi*440*float64(i)/48000))
	}
	p := filepath.Join(t.TempDir(), "speech.wav")
	require.NoError(t, audio.WriteWAV(p, audio.PCM{Rate: 48000, Samples: s}))
	return p
}

// ---- tests: flags -----------------------------------------------------------------------

func TestFlagDefaults(t *testing.T) {
	g := defaultGlobals(func(string) string { return "" })
	fs := flag.NewFlagSet("x", flag.ContinueOnError)
	g.bind(fs)
	require.NoError(t, fs.Parse(nil))
	assert.Equal(t, "http://localhost:8080", g.APIURL)
	assert.Equal(t, "ws://localhost:8081/api/v1/realtime", g.WSURL)
	assert.Equal(t, "http://localhost:3000", g.Origin)
	assert.False(t, g.JSON)
	assert.Equal(t, 30*time.Second, g.Timeout)

	var cf converseFlags
	cfs := flag.NewFlagSet("converse", flag.ContinueOnError)
	cf.bind(cfs)
	require.NoError(t, cfs.Parse(nil))
	assert.Equal(t, converseFlags{
		think: time.Second, waitAI: true, aiTimeout: 20 * time.Second, end: "after-last",
		sf: sessionFlags{duration: 300, startMode: "USER_FIRST"},
	}, cf, "every converse flag at its spec default (§14.11.2)")
}

func TestFlagEnvAndPrecedence(t *testing.T) {
	env := map[string]string{"KOTOKI_CLI_API_URL": "http://env-api", "KOTOKI_CLI_WS_URL": "ws://env-ws", "KOTOKI_CLI_ORIGIN": "http://env-origin"}
	g := defaultGlobals(func(k string) string { return env[k] })
	assert.Equal(t, "http://env-api", g.APIURL)
	assert.Equal(t, "ws://env-ws", g.WSURL)
	assert.Equal(t, "http://env-origin", g.Origin)

	fs := flag.NewFlagSet("x", flag.ContinueOnError)
	g.bind(fs)
	require.NoError(t, fs.Parse([]string{"--api-url", "http://flag-api", "--json", "--timeout", "5s"}))
	assert.Equal(t, "http://flag-api", g.APIURL, "a flag beats the environment")
	assert.Equal(t, "ws://env-ws", g.WSURL)
	assert.True(t, g.JSON)
	assert.Equal(t, 5*time.Second, g.Timeout)
}

func TestGlobalFlagsWorkBeforeAndAfterTheSubcommand(t *testing.T) {
	b := newBackend(t, "a")
	c := newCLI(t, nil)
	c.mustRun("--api-url", b.srv.URL, "login", "--email", "e@x.test", "--password", "pw")
	c.mustRun("balance", "--api-url", b.srv.URL)
	assert.Equal(t, 1, b.hitCount("GET /api/v1/balance"))
}

func TestHelpAndUsageErrors(t *testing.T) {
	c := newCLI(t, nil)
	code, _, errw := c.run("--help")
	assert.Equal(t, 0, code)
	assert.Contains(t, errw, "converse")
	code, _, _ = c.run()
	assert.Equal(t, 2, code)
	code, _, errw = c.run("frobnicate")
	assert.Equal(t, 2, code)
	assert.Contains(t, errw, "unknown subcommand")
	code, _, _ = c.run("login", "--nope")
	assert.Equal(t, 2, code)
}

// ---- tests: credentials -----------------------------------------------------------------

func TestCredentialsFileMode0600(t *testing.T) {
	b := newBackend(t, "a")
	c := newCLI(t, b)
	c.login()
	info, err := os.Stat(c.credPath())
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	dir, err := os.Stat(filepath.Dir(c.credPath()))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o700), dir.Mode().Perm())

	// A file that is too open (made by hand, or by another tool) is tightened on the next use.
	require.NoError(t, os.Chmod(c.credPath(), 0o644))
	c.login()
	info, err = os.Stat(c.credPath())
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())

	f := c.creds().Servers[b.srv.URL]
	assert.Equal(t, "tok-a", f.AccessToken)
	assert.Equal(t, "refresh-a", f.RefreshToken)
}

func TestCredentialsFallBackToHomeConfig(t *testing.T) {
	b := newBackend(t, "a")
	home := t.TempDir()
	c := newCLI(t, b)
	delete(c.env, "XDG_CONFIG_HOME")
	c.env["HOME"] = home
	c.login()
	assert.FileExists(t, filepath.Join(home, ".config", "kotoki-cli", "credentials.json"))
}

func TestCredentialsKeyedByAPIURL(t *testing.T) {
	a, b := newBackend(t, "a"), newBackend(t, "b")
	c := newCLI(t, a)
	c.login()
	c.env["KOTOKI_CLI_API_URL"] = b.srv.URL
	c.login()

	f := c.creds()
	require.Len(t, f.Servers, 2)
	assert.Equal(t, "tok-a", f.Servers[a.srv.URL].AccessToken)
	assert.Equal(t, "tok-b", f.Servers[b.srv.URL].AccessToken)

	// Each server only ever sees its own token.
	c.mustRun("balance")
	c.env["KOTOKI_CLI_API_URL"] = a.srv.URL
	c.mustRun("balance")
	assert.Equal(t, []string{"Bearer tok-b"}, b.auth["/api/v1/balance"])
	assert.Equal(t, []string{"Bearer tok-a"}, a.auth["/api/v1/balance"])

	// A trailing slash is the same server; logging in again leaves the other entry alone.
	c.env["KOTOKI_CLI_API_URL"] = a.srv.URL + "/"
	c.login()
	f = c.creds()
	require.Len(t, f.Servers, 2)
	assert.Equal(t, "tok-b", f.Servers[b.srv.URL].AccessToken)
}

func TestCommandWithoutLoginSendsNoToken(t *testing.T) {
	b := newBackend(t, "a")
	c := newCLI(t, b)
	c.mustRun("catalog")
	assert.Equal(t, []string{""}, b.auth["/api/v1/topics"])
	assert.NoFileExists(t, c.credPath(), "nothing to store, nothing written")
}

func TestRefreshesOn401AndStoresTheRotation(t *testing.T) {
	b := newBackend(t, "a")
	b.validAccess, b.validRefr = "tok-refreshed", "refresh-old"
	c := newCLI(t, b)
	require.NoError(t, c.store().save(b.srv.URL, serverCreds{AccessToken: "stale", RefreshToken: "refresh-old"}))

	out := c.mustRun("balance")
	assert.Contains(t, out, "available 3600s")
	assert.Equal(t, []string{"Bearer stale", "Bearer tok-refreshed"}, b.auth["/api/v1/balance"])
	got := c.creds().Servers[b.srv.URL]
	assert.Equal(t, "tok-refreshed", got.AccessToken)
	assert.Equal(t, "r-rotated", got.RefreshToken, "the rotated refresh token replaces the old one")
}

func TestRefreshFailureAsksForLogin(t *testing.T) {
	b := newBackend(t, "a")
	b.validAccess, b.validRefr = "tok-refreshed", "something-else"
	c := newCLI(t, b)
	require.NoError(t, c.store().save(b.srv.URL, serverCreds{AccessToken: "stale", RefreshToken: "refresh-old"}))
	code, _, errw := c.run("balance")
	assert.Equal(t, 1, code)
	assert.Contains(t, errw, "kotoki-cli login")
}

func TestCorruptCredentialsAreReportedNotOverwritten(t *testing.T) {
	b := newBackend(t, "a")
	c := newCLI(t, b)
	require.NoError(t, os.MkdirAll(filepath.Dir(c.credPath()), 0o700))
	require.NoError(t, os.WriteFile(c.credPath(), []byte("{nope"), 0o600))
	code, _, errw := c.run("balance")
	assert.Equal(t, 1, code)
	assert.Contains(t, errw, "not valid JSON")
	raw, _ := os.ReadFile(c.credPath())
	assert.Equal(t, "{nope", string(raw))
}

// ---- tests: auth commands ---------------------------------------------------------------

func TestRegisterUsesTermsVersion(t *testing.T) {
	b := newBackend(t, "a")
	c := newCLI(t, b)
	c.mustRun("register", "--email", "dev@example.test", "--password", "dev-password-123")
	assert.Equal(t, 1, b.hitCount("GET /api/v1/auth/terms"), "the current terms come from the server")
	assert.Equal(t, "2026-10", b.registerReq["terms_version"])
	assert.Equal(t, "dev@example.test", b.registerReq["email"])
	assert.Equal(t, "dev", b.registerReq["full_name"], "display name defaults to the email's local part")
	assert.Equal(t, "tok-a", c.creds().Servers[b.srv.URL].AccessToken, "register signs in")

	c.mustRun("register", "--email", "x@example.test", "--password", "pw", "--display-name", "Hana", "--terms-version", "2025-01")
	assert.Equal(t, 1, b.hitCount("GET /api/v1/auth/terms"), "an explicit version needs no lookup")
	assert.Equal(t, "2025-01", b.registerReq["terms_version"])
	assert.Equal(t, "Hana", b.registerReq["full_name"])
}

func TestAuthFlagsAreRequired(t *testing.T) {
	b := newBackend(t, "a")
	c := newCLI(t, b)
	for _, args := range [][]string{{"register", "--email", "a@x"}, {"register", "--password", "p"}, {"login", "--email", "a@x"}} {
		code, _, errw := c.run(args...)
		assert.Equal(t, 2, code, args)
		assert.Contains(t, errw, "required")
	}
	assert.Zero(t, b.totalHits())
}

func TestCatalogAndBalanceOutput(t *testing.T) {
	b := newBackend(t, "a")
	c := newCLI(t, b)
	c.login()
	out := c.mustRun("catalog")
	assert.Contains(t, out, testCharacter.String())
	assert.Contains(t, out, "order_coffee")
	assert.Equal(t, "available 3600s, reserved 0s\n", c.mustRun("balance"))
}

// ---- tests: session ---------------------------------------------------------------------

func TestSessionCreateDefaultsAndSelection(t *testing.T) {
	b := newBackend(t, "a")
	c := newCLI(t, b)
	c.login()
	out := c.mustRun("session", "create")
	assert.Equal(t, testSession.String()+"\n", out, "create prints the session id")
	assert.Equal(t, testScenario.String(), b.createReq["scenario_id"], "first active scenario")
	assert.Equal(t, testCharacter.String(), b.createReq["character_id"])
	assert.Equal(t, testTopic.String(), b.createReq["topic_id"], "the topic is the scenario's")
	assert.EqualValues(t, 300, b.createReq["duration_seconds"])
	assert.Equal(t, "USER_FIRST", b.createReq["conversation_start_mode"])
	assert.NotEmpty(t, b.createKey, "Idempotency-Key")

	c.mustRun("session", "create", "--scenario", "ask_way", "--character", testChar2.String(), "--duration", "600", "--start-mode", "AI_FIRST")
	assert.Equal(t, testScenario2.String(), b.createReq["scenario_id"], "selected by code")
	assert.Equal(t, testChar2.String(), b.createReq["character_id"], "selected by id")
	assert.EqualValues(t, 600, b.createReq["duration_seconds"])
	assert.Equal(t, "AI_FIRST", b.createReq["conversation_start_mode"])
}

func TestSessionCreateValidatesBeforeTheNetwork(t *testing.T) {
	b := newBackend(t, "a")
	c := newCLI(t, b)
	for _, args := range [][]string{
		{"session", "create", "--duration", "301"},
		{"session", "create", "--start-mode", "NOPE"},
	} {
		code, _, _ := c.run(args...)
		assert.Equal(t, 2, code, args)
	}
	assert.Zero(t, b.totalHits())
	code, _, errw := c.run("session", "create", "--scenario", "nope")
	assert.Equal(t, 1, code)
	assert.Contains(t, errw, "no active scenario")
	assert.Zero(t, b.hitCount("POST /api/v1/sessions"))
}

func TestSessionGetCancelTicketAndJSON(t *testing.T) {
	b := newBackend(t, "a")
	c := newCLI(t, b)
	c.login()
	assert.Contains(t, c.mustRun("session", "get", "--session", testSession.String()), "RESERVED")
	assert.Equal(t, "cancelled "+testSession.String()+"\n", c.mustRun("session", "cancel", "--session", testSession.String()))
	assert.Equal(t, "tkt-a\n", c.mustRun("ticket", "--session", testSession.String()))

	var got map[string]any
	require.NoError(t, json.Unmarshal([]byte(c.mustRun("--json", "session", "get", "--session", testSession.String())), &got))
	assert.Equal(t, "RESERVED", got["status"])

	code, _, errw := c.run("session", "get")
	assert.Equal(t, 2, code)
	assert.Contains(t, errw, "--session")
	code, _, _ = c.run("session", "get", "--session", "not-a-uuid")
	assert.Equal(t, 2, code)
}

func TestSessionTranscriptPages(t *testing.T) {
	b := newBackend(t, "a")
	c := newCLI(t, b)
	c.login()
	out := c.mustRun("session", "transcript", "--session", testSession.String())
	assert.Contains(t, out, "#1 [COMPLETED]")
	assert.Contains(t, out, "user: はじめまして")
	assert.Contains(t, out, "ai:   こんにちは")
	assert.Contains(t, out, "#2 [COMPLETED]")
	require.Len(t, b.turnQueries, 2)
	assert.NotContains(t, b.turnQueries[0], "cursor")
	assert.Contains(t, b.turnQueries[1], "cursor=", "the second page continues after the last turn")
}

func TestSessionEndOverWebSocket(t *testing.T) {
	b := newBackend(t, "a")
	b.startMode = "RESUME" // the fake server expects session.resume
	c := newCLI(t, b)
	c.login()
	out := c.mustRun("session", "end", "--session", testSession.String())
	assert.Contains(t, out, "COMPLETED")
	assert.Contains(t, out, "charged_seconds=7")
	conns := b.conns()
	require.Len(t, conns, 1)
	assert.Equal(t, "tkt-a", conns[0].ticket)
	assert.Equal(t, "http://app.test", conns[0].origin, "the --origin header is sent")
	require.Len(t, conns[0].events, 2)
	assert.Equal(t, "session.resume", conns[0].events[0].Type)
	assert.Equal(t, "session.end", conns[0].events[1].Type)
	assert.Equal(t, protocol.SessionEnd{Reason: "USER_ENDED"}, conns[0].events[1].Payload)
}

func TestSessionEndRefusesAReservedSession(t *testing.T) {
	b := newBackend(t, "a") // the fake asks for session.start: the session never started
	c := newCLI(t, b)
	c.login()
	code, _, errw := c.run("session", "end", "--session", testSession.String())
	assert.Equal(t, 1, code)
	assert.Contains(t, errw, "session cancel")
	for _, ev := range b.conns()[0].events {
		assert.NotEqual(t, "session.end", ev.Type)
	}
}

func TestEvaluationGetWaitsUntilCompleted(t *testing.T) {
	old := pollInterval
	pollInterval = time.Millisecond
	t.Cleanup(func() { pollInterval = old })
	b := newBackend(t, "a")
	b.evalStatus = []string{"PENDING", "PENDING", "COMPLETED"}
	c := newCLI(t, b)
	c.login()
	out := c.mustRun("evaluation", "get", "--session", testSession.String(), "--wait", "5s")
	assert.Contains(t, out, `"status": "COMPLETED"`)
	assert.Equal(t, 3, b.hitCount("GET /api/v1/sessions/"+testSession.String()+"/evaluation"))

	b.mu.Lock()
	b.evalStatus = []string{"PENDING"}
	b.mu.Unlock()
	code, _, errw := c.run("evaluation", "get", "--session", testSession.String(), "--wait", "20ms")
	assert.Equal(t, 1, code)
	assert.Contains(t, errw, "PENDING")
	// without --wait: one look, whatever the status
	assert.Contains(t, c.mustRun("evaluation", "get", "--session", testSession.String()), "PENDING")
}

// ---- tests: converse --------------------------------------------------------------------

func TestConverseRequiresWav(t *testing.T) {
	b := newBackend(t, "a")
	c := newCLI(t, b)
	c.login()
	before := b.totalHits()
	for _, args := range [][]string{{"converse"}, {"chat"}, {"converse", "--wav", " , "}} {
		code, _, errw := c.run(args...)
		assert.Equal(t, 2, code, args)
		assert.Contains(t, errw, "--wav is required")
	}
	assert.Equal(t, before, b.totalHits(), "nothing is created before the flags are valid")
	code, _, _ := c.run("converse", "--wav", filepath.Join(t.TempDir(), "missing.wav"))
	assert.Equal(t, 1, code)
	assert.Equal(t, before, b.totalHits(), "an unreadable WAV fails before any session is created")
	code, _, errw := c.run("converse", "--wav", speechWAV(t, 100*time.Millisecond), "--end", "soon")
	assert.Equal(t, 2, code)
	assert.Contains(t, errw, "--end")
	code, _, errw = c.run("converse", "--wav", speechWAV(t, 100*time.Millisecond), "--duration", "301")
	assert.Equal(t, 2, code)
	assert.Contains(t, errw, "--duration")
	assert.Equal(t, before, b.totalHits(), "invalid session flags are caught before the network")
}

type eventLine struct {
	ReceivedAt time.Time       `json:"received_at"`
	Event      json.RawMessage `json:"event"`
}

func TestConverseOutputs(t *testing.T) {
	b := newBackend(t, "a")
	c := newCLI(t, b)
	c.login()
	dir := filepath.Join(t.TempDir(), "out")
	wav := speechWAV(t, 200*time.Millisecond)
	out := c.mustRun("converse", "--wav", wav+","+wav, "--think", "20ms", "--out-dir", dir)

	// WAV per turn and generation: PCM16 mono 48 kHz, three 20 ms frames
	for _, name := range []string{"turn-001-gen-1.wav", "turn-002-gen-2.wav"} {
		raw, err := os.ReadFile(filepath.Join(dir, name))
		require.NoError(t, err, name)
		require.Equal(t, "RIFF", string(raw[:4]))
		require.Equal(t, "WAVE", string(raw[8:12]))
		assert.EqualValues(t, 1, binary.LittleEndian.Uint16(raw[20:]), "PCM")
		assert.EqualValues(t, 1, binary.LittleEndian.Uint16(raw[22:]), "mono")
		assert.EqualValues(t, 48000, binary.LittleEndian.Uint32(raw[24:]), "48 kHz")
		assert.EqualValues(t, 16, binary.LittleEndian.Uint16(raw[34:]), "16 bit")
		assert.Equal(t, 3*960*2, len(raw)-44, "three decoded 20 ms frames")
		pcm, err := audio.ReadWAV(filepath.Join(dir, name))
		require.NoError(t, err)
		assert.Equal(t, 48000, pcm.Rate)
	}

	// events.jsonl: one JSON object per line, each with its receive time
	f, err := os.Open(filepath.Join(dir, "events.jsonl"))
	require.NoError(t, err)
	defer func() { _ = f.Close() }()
	var types []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var l eventLine
		require.NoError(t, json.Unmarshal(sc.Bytes(), &l), sc.Text())
		assert.False(t, l.ReceivedAt.IsZero(), "receive timestamp")
		var ev struct {
			Type string `json:"type"`
		}
		require.NoError(t, json.Unmarshal(l.Event, &ev))
		types = append(types, ev.Type)
	}
	require.NoError(t, sc.Err())
	assert.Contains(t, types, "session.started")
	assert.Equal(t, 2, countOf(types, "turn.completed"))

	// summary.json
	var sum struct {
		SessionID     string  `json:"session_id"`
		TurnLatencyMS []int64 `json:"turn_latency_ms"`
		Status        string  `json:"status"`
		Charged       int     `json:"charged_seconds"`
		StartMS       *int64  `json:"start_ms"`
	}
	raw, err := os.ReadFile(filepath.Join(dir, "summary.json"))
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, &sum))
	assert.Equal(t, testSession.String(), sum.SessionID)
	assert.Len(t, sum.TurnLatencyMS, 2, "one latency per utterance")
	assert.Equal(t, "COMPLETED", sum.Status)
	assert.Equal(t, 7, sum.Charged)
	assert.NotNil(t, sum.StartMS)

	for _, name := range []string{"events.jsonl", "summary.json", "turn-001-gen-1.wav"} {
		info, err := os.Stat(filepath.Join(dir, name))
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o600), info.Mode().Perm(), name+": transcripts are private")
	}

	assert.Contains(t, out, "--- summary ---")
	assert.Contains(t, out, "turns=2")
	assert.Contains(t, out, "session COMPLETED  charged_seconds=7")
	assert.Contains(t, out, "turn.completed")
	assert.Contains(t, out, `"こんにちは"`, "the transcript is printed locally")

	conns := b.conns()
	require.Len(t, conns, 1)
	assert.Equal(t, "http://app.test", conns[0].origin)
	assert.Equal(t, "session.start", conns[0].events[0].Type)
	assert.Equal(t, "session.end", conns[0].events[len(conns[0].events)-1].Type, "--end after-last ends the session")
}

func countOf(s []string, v string) int {
	n := 0
	for _, x := range s {
		if x == v {
			n++
		}
	}
	return n
}

func TestConverseDefaultOutDirAndFlags(t *testing.T) {
	b := newBackend(t, "a")
	c := newCLI(t, b)
	c.login()
	wav := speechWAV(t, 200*time.Millisecond)
	tmp := t.TempDir()
	t.Chdir(tmp)

	out := c.mustRun("converse", "--wav", wav, "--quiet", "--no-audio-save",
		"--duration", "600", "--scenario", "ask_way", "--character", "ken")
	assert.NotContains(t, out, "← ", "--quiet prints the summary only")
	dir := filepath.Join(tmp, "out", testSession.String())
	assert.FileExists(t, filepath.Join(dir, "summary.json"))
	assert.FileExists(t, filepath.Join(dir, "events.jsonl"))
	matches, _ := filepath.Glob(filepath.Join(dir, "*.wav"))
	assert.Empty(t, matches, "--no-audio-save writes no WAV")
	assert.EqualValues(t, 600, b.createReq["duration_seconds"])
	assert.Equal(t, testScenario2.String(), b.createReq["scenario_id"])
	assert.Equal(t, testChar2.String(), b.createReq["character_id"])
}

func TestConverseWithExistingSession(t *testing.T) {
	b := newBackend(t, "a")
	c := newCLI(t, b)
	c.login()
	c.mustRun("converse", "--session", testSession.String(), "--wav", speechWAV(t, 200*time.Millisecond), "--quiet", "--out-dir", t.TempDir())
	assert.Zero(t, b.hitCount("POST /api/v1/sessions"), "no new session")
	assert.Equal(t, 1, b.hitCount("GET /api/v1/sessions/"+testSession.String()))
}

func TestConverseAIFirstWaitsForTheGreeting(t *testing.T) {
	b := newBackend(t, "a")
	c := newCLI(t, b)
	c.login()
	dir := t.TempDir()
	c.mustRun("converse", "--wav", speechWAV(t, 200*time.Millisecond), "--start-mode", "AI_FIRST", "--quiet", "--out-dir", dir)
	// the greeting is turn 1 / generation 1; the answer to the user is turn 2 / generation 2
	assert.FileExists(t, filepath.Join(dir, "turn-001-gen-1.wav"))
	assert.FileExists(t, filepath.Join(dir, "turn-002-gen-2.wav"))
	var sum struct {
		TurnLatencyMS []int64 `json:"turn_latency_ms"`
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "summary.json"))
	require.NoError(t, json.Unmarshal(raw, &sum))
	assert.Len(t, sum.TurnLatencyMS, 1, "the greeting is not taken for the answer")
}

func TestConverseDropAndResume(t *testing.T) {
	b := newBackend(t, "a")
	c := newCLI(t, b)
	c.login()
	dir := t.TempDir()
	// the utterance lasts 400 ms; the connection drops 100 ms in, mid-utterance
	c.mustRun("converse", "--wav", speechWAV(t, 400*time.Millisecond), "--drop-after", "100ms", "--drop-for", "50ms", "--quiet", "--out-dir", dir)
	conns := b.conns()
	require.Len(t, conns, 2)
	assert.Equal(t, "session.start", conns[0].events[0].Type)
	assert.Equal(t, "session.resume", conns[1].events[0].Type)
	assert.Equal(t, "tkt-a", conns[1].ticket)
	var sum struct {
		ResumeMS []int64 `json:"resume_ms"`
		Status   string  `json:"status"`
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "summary.json"))
	require.NoError(t, json.Unmarshal(raw, &sum))
	assert.Len(t, sum.ResumeMS, 1)
	assert.Equal(t, "COMPLETED", sum.Status)
}

func TestJSONOutputMode(t *testing.T) {
	b := newBackend(t, "a")
	c := newCLI(t, b)
	out := c.mustRun("--json", "login", "--email", "dev@example.test", "--password", "pw")
	var login map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &login))
	assert.Equal(t, "dev@example.test", login["email"])

	var bal map[string]any
	require.NoError(t, json.Unmarshal([]byte(c.mustRun("--json", "balance")), &bal))
	assert.EqualValues(t, 3600, bal["available_seconds"])

	out = c.mustRun("--json", "converse", "--wav", speechWAV(t, 200*time.Millisecond), "--out-dir", t.TempDir())
	lines := strings.Split(strings.TrimSpace(out), "\n")
	require.Greater(t, len(lines), 3)
	for _, l := range lines {
		var m map[string]any
		require.NoError(t, json.Unmarshal([]byte(l), &m), "every stdout line is JSON: %q", l)
	}
	var first, last map[string]any
	require.NoError(t, json.Unmarshal([]byte(lines[0]), &first))
	require.NoError(t, json.Unmarshal([]byte(lines[len(lines)-1]), &last))
	assert.Equal(t, "event", first["kind"])
	assert.Equal(t, "summary", last["kind"])
	assert.Contains(t, last, "turn_latency_ms")
	assert.Equal(t, "COMPLETED", last["status"])
	assert.NotContains(t, out, "--- summary ---")
}

// ---- tests: the script ------------------------------------------------------------------

func pk(n int) [][]byte { return make([][]byte, n) }

func basePlan() plan {
	return plan{
		utterances: [][][]byte{pk(10), pk(20)}, labels: []string{"a.wav", "b.wav"},
		think: time.Second, waitAI: true, aiTimeout: 20 * time.Second, end: endMode{kind: endAfterLast},
		sessionLen: 300 * time.Second,
	}
}

func TestBuildStepsUserFirst(t *testing.T) {
	to := 20 * time.Second
	assert.Equal(t, []kotokiclient.Step{
		kotokiclient.Speak{Packets: pk(10), Label: "a.wav"},
		kotokiclient.WaitAIDone{Timeout: to},
		kotokiclient.Think{D: time.Second},
		kotokiclient.Speak{Packets: pk(20), Label: "b.wav"},
		kotokiclient.WaitAIDone{Timeout: to},
		kotokiclient.End{Timeout: to},
	}, buildSteps(basePlan()))
}

func TestBuildStepsAIFirstStartsByWaiting(t *testing.T) {
	p := basePlan()
	p.aiFirst = true
	steps := buildSteps(p)
	assert.Equal(t, kotokiclient.WaitAIDone{Timeout: 20 * time.Second}, steps[0])
	assert.IsType(t, kotokiclient.Speak{}, steps[1])
}

func TestBuildStepsNoWaitAI(t *testing.T) {
	p := basePlan()
	p.waitAI = false
	steps := buildSteps(p)
	assert.Equal(t, kotokiclient.Think{D: time.Second}, steps[1], "no WaitAIDone between utterances")
	p.think = 0
	steps = buildSteps(p)
	assert.IsType(t, kotokiclient.Speak{}, steps[1], "no think either")
}

func TestBuildStepsBargeIn(t *testing.T) {
	p := basePlan()
	p.bargeAfter = 300 * time.Millisecond
	assert.Equal(t, []kotokiclient.Step{
		kotokiclient.Speak{Packets: pk(10), Label: "a.wav"},
		kotokiclient.BargeIn{After: 300 * time.Millisecond, Packets: pk(20), Timeout: 20 * time.Second},
		kotokiclient.WaitAIDone{Timeout: 20 * time.Second},
		kotokiclient.End{Timeout: 20 * time.Second},
	}, buildSteps(p), "the 2nd utterance barges in; nothing waits before it")
}

func TestBuildStepsEndModes(t *testing.T) {
	p := basePlan()
	p.end = endMode{kind: endWaitExpiry}
	steps := buildSteps(p)
	assert.Equal(t, kotokiclient.Think{D: 330 * time.Second}, steps[len(steps)-1], "session length plus margin")
	assert.NotContains(t, steps, kotokiclient.End{Timeout: 20 * time.Second})

	p.end = endMode{kind: endAfterDuration, d: 90 * time.Second}
	steps = buildSteps(p)
	assert.Equal(t, []kotokiclient.Step{kotokiclient.Think{D: 90 * time.Second}, kotokiclient.End{Timeout: 20 * time.Second}}, steps[len(steps)-2:])
}

func TestParseEnd(t *testing.T) {
	m, err := parseEnd("after-last")
	require.NoError(t, err)
	assert.Equal(t, endAfterLast, m.kind)
	m, err = parseEnd("wait-expiry")
	require.NoError(t, err)
	assert.Equal(t, endWaitExpiry, m.kind)
	m, err = parseEnd("90s")
	require.NoError(t, err)
	assert.Equal(t, endMode{kind: endAfterDuration, d: 90 * time.Second}, m)
	for _, bad := range []string{"", "soon", "0s", "-5s"} {
		_, err = parseEnd(bad)
		assert.Error(t, err, bad)
	}
}

func TestInsertDrop(t *testing.T) {
	to := 20 * time.Second
	drop := kotokiclient.Drop{For: 2 * time.Second}
	end := kotokiclient.End{Timeout: to}

	t.Run("inside a Think", func(t *testing.T) {
		in := []kotokiclient.Step{kotokiclient.Speak{Packets: pk(50)}, kotokiclient.Think{D: 3 * time.Second}, end} // speech 1 s
		got := insertDrop(in, 2*time.Second, 2*time.Second)
		assert.Equal(t, []kotokiclient.Step{kotokiclient.Speak{Packets: pk(50)}, kotokiclient.Think{D: time.Second}, drop, kotokiclient.Think{D: 2 * time.Second}, end}, got)
	})
	t.Run("inside a Speak", func(t *testing.T) {
		in := []kotokiclient.Step{kotokiclient.Speak{Packets: pk(50), Label: "x"}, end}
		got := insertDrop(in, 300*time.Millisecond, 2*time.Second)
		assert.Equal(t, []kotokiclient.Step{
			kotokiclient.Speak{Packets: pk(15), Label: "x"}, drop, kotokiclient.Speak{Packets: pk(35), Label: "x"}, end,
		}, got)
	})
	t.Run("at the very start", func(t *testing.T) {
		in := []kotokiclient.Step{kotokiclient.Speak{Packets: pk(50)}, end}
		got := insertDrop(in, 5*time.Millisecond, 2*time.Second)
		assert.Equal(t, []kotokiclient.Step{drop, kotokiclient.Speak{Packets: pk(50)}, end}, got)
	})
	t.Run("on a boundary", func(t *testing.T) {
		in := []kotokiclient.Step{kotokiclient.Speak{Packets: pk(50)}, kotokiclient.WaitAIDone{Timeout: to}, end}
		got := insertDrop(in, time.Second, 2*time.Second)
		assert.Equal(t, []kotokiclient.Step{kotokiclient.Speak{Packets: pk(50)}, drop, kotokiclient.WaitAIDone{Timeout: to}, end}, got)
	})
	t.Run("after a BargeIn that spans it", func(t *testing.T) {
		b := kotokiclient.BargeIn{After: time.Second, Packets: pk(10), Timeout: to}
		got := insertDrop([]kotokiclient.Step{b, end}, 500*time.Millisecond, 2*time.Second)
		assert.Equal(t, []kotokiclient.Step{b, drop, end}, got)
	})
	t.Run("later than the script: before the End", func(t *testing.T) {
		in := []kotokiclient.Step{kotokiclient.Speak{Packets: pk(10)}, end}
		got := insertDrop(in, time.Hour, 2*time.Second)
		assert.Equal(t, []kotokiclient.Step{kotokiclient.Speak{Packets: pk(10)}, drop, end}, got)
	})
	t.Run("no End: appended", func(t *testing.T) {
		in := []kotokiclient.Step{kotokiclient.Speak{Packets: pk(10)}}
		assert.Equal(t, []kotokiclient.Step{kotokiclient.Speak{Packets: pk(10)}, drop}, insertDrop(in, time.Hour, 2*time.Second))
	})
	t.Run("one drop only", func(t *testing.T) {
		in := []kotokiclient.Step{kotokiclient.Think{D: 10 * time.Second}, kotokiclient.Think{D: 10 * time.Second}, end}
		n := 0
		for _, s := range insertDrop(in, time.Second, time.Second) {
			if _, ok := s.(kotokiclient.Drop); ok {
				n++
			}
		}
		assert.Equal(t, 1, n)
	})
	t.Run("through buildSteps", func(t *testing.T) {
		p := basePlan()
		p.dropAfter, p.dropFor = 100*time.Millisecond, time.Second
		steps := buildSteps(p)
		assert.Equal(t, kotokiclient.Speak{Packets: pk(5), Label: "a.wav"}, steps[0])
		assert.Equal(t, kotokiclient.Drop{For: time.Second}, steps[1])
		p.dropAfter = 0
		for _, s := range buildSteps(p) {
			assert.NotEqual(t, kotokiclient.Drop{For: time.Second}, s, "drop-after 0 is off")
		}
	})
}

func TestPercentile(t *testing.T) {
	v := []int64{500, 100, 300, 200, 400}
	assert.EqualValues(t, 300, percentile(v, 0.5))
	assert.EqualValues(t, 500, percentile(v, 0.95))
	assert.EqualValues(t, 0, percentile(nil, 0.5))
	assert.Equal(t, "-", stat(nil))
}
