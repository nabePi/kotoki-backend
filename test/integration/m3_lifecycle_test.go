//go:build integration

package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/admin"
	"github.com/nabePi/kotoki-backend/internal/auth"
	"github.com/nabePi/kotoki-backend/internal/balance"
	"github.com/nabePi/kotoki-backend/internal/content"
	"github.com/nabePi/kotoki-backend/internal/evaluation"
	"github.com/nabePi/kotoki-backend/internal/httpapi"
	"github.com/nabePi/kotoki-backend/internal/mailer"
	"github.com/nabePi/kotoki-backend/internal/platform/clock"
	"github.com/nabePi/kotoki-backend/internal/platform/idgen"
	"github.com/nabePi/kotoki-backend/internal/platform/metrics"
	"github.com/nabePi/kotoki-backend/internal/platform/ratelimit"
	"github.com/nabePi/kotoki-backend/internal/postgres"
	"github.com/nabePi/kotoki-backend/internal/redisstore"
	"github.com/nabePi/kotoki-backend/internal/session"
	"github.com/nabePi/kotoki-backend/internal/testutil"
	"github.com/nabePi/kotoki-backend/internal/testutil/fakes"
)

const lifecycleTerms = "2026-10-01"

var jobInterviewTopic = uuid.MustParse("01920000-0001-7000-8000-000000000001")

// lifecycle wires the real PostgreSQL adapters, the real services and the real router, the way
// cmd/api does, over a seeded catalog; only the learner adaptive provider is the stub it is in M3.
type lifecycle struct {
	t       *testing.T
	pool    *pgxpool.Pool
	srv     *httptest.Server
	clk     *clock.Fake
	store   *postgres.Store
	cached  content.Repository
	svc     *session.Service
	runtime *session.RuntimeService
	query   *session.QueryService
	admin   *content.AdminService
	hb      *fakes.Heartbeats
	sweeper *session.Sweeper
	logs    *bytes.Buffer
	users   int
}

// allowAll lets every request through: the scenarios register many users from one address.
type allowAll struct{}

func (allowAll) Allow(context.Context, ratelimit.Policy, string) (ratelimit.Decision, error) {
	return ratelimit.Decision{Allowed: true, Remaining: 1}, nil
}

type openingAdapter struct{ repo content.Repository }

func (o openingAdapter) PublishedPromptVersionID(ctx context.Context, key string) (*uuid.UUID, error) {
	pv, err := o.repo.GetPublishedPromptVersion(ctx, content.PromptKey(key))
	if errors.Is(err, content.ErrNotFound) {
		return nil, nil //nolint:nilnil // no PUBLISHED version is a normal answer
	}
	if err != nil {
		return nil, err
	}
	return &pv.ID, nil
}

func newLifecycle(t *testing.T) *lifecycle {
	t.Helper()
	pool := seededPool(t)
	rdb := testutil.NewRedis(t)
	ids := idgen.NewV7()
	clk := clock.NewFake(time.Now().UTC().Truncate(time.Millisecond))
	logs := &bytes.Buffer{}
	log := slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	store := postgres.NewStore(pool, postgres.WithIDs(ids), postgres.WithClock(clk))
	reg := prometheus.NewRegistry()
	t.Cleanup(func() { testutil.AssertReconciled(t, pool) })

	tokens, err := auth.NewJWTIssuer(auth.JWTConfig{Secret: []byte(strings.Repeat("l", 32)), Issuer: "kotoki-api", Audience: "kotoki", TTL: 15 * time.Minute}, ids)
	require.NoError(t, err)
	hasher := auth.NewArgon2Hasher(auth.Argon2Config{MemoryKiB: 64, Iterations: 1, Parallelism: 1, MaxConcurrent: 8, MaxWait: 2 * time.Second})
	dispatcher := auth.NewMailDispatcher(mailer.NewFake(), log, auth.DispatcherOptions{})
	t.Cleanup(func() { _ = dispatcher.Close(context.Background()) })
	balSvc := balance.NewService(store.BalanceStore(), clk, ids)
	adminSvc := admin.NewService(store.AdminStore(), balSvc, clk, ids, log)
	authSvc, err := auth.NewService(store.AuthStore(), hasher, tokens, dispatcher, clk, ids, log, metrics.NewAuth(reg), auth.Config{
		TermsVersion: lifecycleTerms, InitialBalanceSeconds: 1200, AccessTTL: 15 * time.Minute, RefreshTTL: 720 * time.Hour, RefreshReuseGrace: 10 * time.Second,
		ResetTTL: 15 * time.Minute, ResetURLBase: "https://app.example/reset-password"})
	require.NoError(t, err)
	userSvc := auth.NewUserService(store.AuthStore(), hasher, adminSvc, clk, log, lifecycleTerms)

	cached := content.NewCachedRepository(postgres.NewContentRepo(pool), 30*time.Second, clk)
	vcfg := content.ValidatorConfig{TTSEnabled: func(string) bool { return true }}
	rcfg := content.ResolverConfig{TTSMode: "fake", TTSEnabled: func(string) bool { return true }, LLMConfigured: true}
	resolver := content.NewResolver(cached, rcfg, nil)
	contentAdmin := content.NewAdminService(content.AdminDeps{Store: store.ContentStore(), Validator: vcfg, Clock: clk, IDs: ids, Invalidator: cached.(content.Invalidator)})
	sessions := session.NewService(session.Deps{
		Store: store.SessionStore(), Reader: postgres.NewSessionRepo(pool), Balance: balSvc, Resolver: resolver, Adaptive: session.NeutralAdaptiveProvider{},
		Users: fakes.NewUserGate(session.UserGateInfo{Status: "ACTIVE", TermsVersion: lifecycleTerms}), OpeningPrompts: openingAdapter{cached},
		Control: redisstore.NewSessionControl(rdb), Clock: clk, IDs: ids, Log: log, TermsVersion: lifecycleTerms, Metrics: metrics.NewSession(reg), ServiceName: "api",
	})
	store.SetSessionCanceller(sessions)
	query := session.NewQueryService(postgres.NewSessionViewRepo(pool), postgres.NewTurnRepo(pool), clk, 180)
	tickets := auth.NewTicketService(store.AuthStore(), postgres.NewTicketSessionReader(pool), redisstore.NewTicketStore(rdb, clk), clk,
		auth.TicketConfig{TTL: 30 * time.Second, RealtimeURL: "wss://realtime.example"})
	hb := fakes.NewHeartbeats()

	router := httpapi.NewRouter(httpapi.Deps{
		Log: log, Clock: clk, Tokens: tokens, Metrics: metrics.NewHTTP(reg, "kotoki-api"),
		Limiter: allowAll{},
		Cfg:     httpapi.Config{CORSOrigins: []string{"http://localhost:3000"}, BodyLimit: 64 << 10, Cookie: httpapi.CookieConfig{Secure: false, MaxAge: 720 * time.Hour}},
		Auth:    authSvc, Users: userSvc, Balance: balSvc, Admin: adminSvc, AdminGate: adminSvc,
		Content: content.NewCatalogService(cached, rcfg), ContentAdmin: contentAdmin, Session: sessions, SessionQuery: query, Tickets: tickets,
		Evaluation:    evaluation.NewQueryService(postgres.NewEvaluationReader(pool)),
		AdminSessions: session.NewAdminQueryService(postgres.NewSessionAdminRepo(pool), clk, 180), AdminAudit: adminSvc,
	})
	srv := httptest.NewServer(router)
	t.Cleanup(srv.Close)
	return &lifecycle{t: t, pool: pool, srv: srv, clk: clk, store: store, cached: cached, svc: sessions, runtime: session.NewRuntimeService(sessions, postgres.NewSessionRepo(pool)),
		query: query, admin: contentAdmin, hb: hb, sweeper: session.NewSweeper(sessions, postgres.NewSessionRepo(pool), hb, clk, log), logs: logs}
}

type lcResp struct {
	code   int
	header http.Header
	body   []byte
}

func (l *lifecycle) call(method, path, body string, headers ...string) lcResp {
	l.t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(context.Background(), method, l.srv.URL+path, rd)
	require.NoError(l.t, err)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	res, err := http.DefaultClient.Do(req)
	require.NoError(l.t, err)
	defer func() { _ = res.Body.Close() }()
	b, err := io.ReadAll(res.Body)
	require.NoError(l.t, err)
	return lcResp{code: res.StatusCode, header: res.Header, body: b}
}

func (r lcResp) json(t *testing.T) map[string]any {
	t.Helper()
	var m map[string]any
	require.NoError(t, json.Unmarshal(r.body, &m), string(r.body))
	return m
}

func (r lcResp) errCode(t *testing.T) string {
	t.Helper()
	e, _ := r.json(t)["error"].(map[string]any)
	code, _ := e["code"].(string)
	return code
}

// newUser registers a user (1200 s of initial balance) and returns the id and the bearer header.
func (l *lifecycle) newUser() (uuid.UUID, string) {
	l.t.Helper()
	l.users++
	email := "user" + string(rune('a'+l.users)) + "@example.com"
	r := l.call(http.MethodPost, "/api/v1/auth/register",
		`{"email":"`+email+`","password":"correct horse battery","full_name":"Hana Tanaka","terms_version":"`+lifecycleTerms+`"}`)
	require.Equal(l.t, http.StatusCreated, r.code, string(r.body))
	tok, _ := r.json(l.t)["access_token"].(string)
	var id uuid.UUID
	require.NoError(l.t, l.pool.QueryRow(context.Background(), `SELECT id FROM users WHERE email = $1`, email).Scan(&id))
	return id, "Bearer " + tok
}

func (l *lifecycle) body(mode string, duration string) string {
	return `{"topic_id":"` + jobInterviewTopic.String() + `","scenario_id":"` + backendID + `","character_id":"` + harukaID + `","duration_seconds":` + duration + `,"conversation_start_mode":"` + mode + `"}`
}

func (l *lifecycle) create(authz, key, mode, duration string) lcResp {
	l.t.Helper()
	return l.call(http.MethodPost, "/api/v1/sessions", l.body(mode, duration), "Authorization", authz, "Idempotency-Key", key)
}

func (l *lifecycle) sessionID(r lcResp) uuid.UUID {
	l.t.Helper()
	id, err := uuid.Parse(r.json(l.t)["id"].(string))
	require.NoError(l.t, err, string(r.body))
	return id
}

func (l *lifecycle) count(query string, args ...any) int {
	l.t.Helper()
	var n int
	require.NoError(l.t, l.pool.QueryRow(context.Background(), query, args...).Scan(&n))
	return n
}

func (l *lifecycle) available(userID uuid.UUID) (available, reserved int64) {
	l.t.Helper()
	require.NoError(l.t, l.pool.QueryRow(context.Background(), `SELECT available_seconds, reserved_seconds FROM balance_accounts WHERE user_id = $1`, userID).Scan(&available, &reserved))
	return available, reserved
}

// start walks a created session through ClaimRuntime, MarkStarting and Activate as the runtime would.
func (l *lifecycle) start(id uuid.UUID) (fence int64, rec session.RuntimeRecord) {
	l.t.Helper()
	ctx := context.Background()
	fence, _, err := l.runtime.ClaimRuntime(ctx, id, []session.Status{session.StatusReserved})
	require.NoError(l.t, err)
	rec, err = l.runtime.MarkStarting(ctx, id, fence)
	require.NoError(l.t, err)
	_, _, err = l.runtime.Activate(ctx, id, fence)
	require.NoError(l.t, err)
	return fence, rec
}

func (l *lifecycle) eventTypes(id uuid.UUID) []string {
	l.t.Helper()
	rows, err := l.pool.Query(context.Background(), `SELECT event_type FROM session_events WHERE session_id = $1 ORDER BY created_at, id`, id)
	require.NoError(l.t, err)
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		require.NoError(l.t, rows.Scan(&s))
		out = append(out, s)
	}
	return out
}

// TestLifecycle runs every scenario of spec 5.11 against one database, each with its own user, and
// ends by proving that nothing is orphaned and the books balance.
func TestLifecycle(t *testing.T) {
	l := newLifecycle(t)
	ctx := context.Background()

	t.Run("happy path AI_FIRST", func(t *testing.T) {
		userID, authz := l.newUser()
		created := l.create(authz, "happy-key-0001", "AI_FIRST", "600")
		require.Equal(t, 201, created.code, string(created.body))
		sid := l.sessionID(created)
		require.Equal(t, "RESERVED", created.json(t)["status"])

		ticket := l.call(http.MethodPost, "/api/v1/sessions/"+sid.String()+"/realtime-ticket", "", "Authorization", authz)
		require.Equal(t, 200, ticket.code, string(ticket.body))
		require.EqualValues(t, 30, ticket.json(t)["expires_in"])
		require.Equal(t, "wss://realtime.example/api/v1/realtime", ticket.json(t)["realtime_url"])

		fence, rec := l.start(sid)
		require.NotNil(t, rec.OpeningPromptVersionID, "the seeded OPENING prompt is PUBLISHED")
		require.Equal(t, session.StartAIFirst, rec.ConversationStartMode)

		l.clk.Advance(420*time.Second + 400*time.Millisecond)
		require.NoError(t, l.runtime.BeginEnding(ctx, sid, fence, session.EndUserEnded))
		l.clk.Advance(3 * time.Second)
		st, err := l.runtime.Complete(ctx, sid, fence)
		require.NoError(t, err)
		require.Equal(t, 421, st.ChargedSeconds, "420.4 s rounds up")
		require.Equal(t, session.EndUserEnded, st.EndReason)

		avail, reserved := l.available(userID)
		require.Equal(t, int64(1200-421), avail)
		require.Zero(t, reserved)
		require.Equal(t, 1, l.count(`SELECT count(*) FROM event_outbox WHERE aggregate_id = $1 AND event_type = 'session.completed' AND payload->>'final_status' = 'COMPLETED'`, sid))
		require.Equal(t, []string{"SESSION_RESERVED", "SESSION_STARTING", "SESSION_STARTED", "SESSION_ENDING", "SESSION_COMPLETED"}, l.eventTypes(sid))

		var code, formality string
		require.NoError(t, l.pool.QueryRow(ctx, `SELECT config_snapshot->'character'->>'code', config_snapshot->>'formality' FROM sessions WHERE id = $1`, sid).Scan(&code, &formality))
		require.Equal(t, "HARUKA", code)
		require.Equal(t, "KEIGO", formality)

		get := l.call(http.MethodGet, "/api/v1/sessions/"+sid.String(), "", "Authorization", authz)
		require.Equal(t, 200, get.code)
		require.Equal(t, "COMPLETED", get.json(t)["status"])
		require.EqualValues(t, 421, get.json(t)["charged_seconds"])
	})

	t.Run("character edited mid session", func(t *testing.T) {
		userID, authz := l.newUser()
		sid := l.sessionID(l.create(authz, "edit-key-0001", "USER_FIRST", "300"))
		fence, rec := l.start(sid)
		before := string(rec.ConfigSnapshot)

		persona := strings.Repeat("Persona yang sudah diganti admin. ", 3)
		disabled := content.StatusDisabled
		adminID := userID // the audit row needs an existing user id (foreign key)
		_, err := l.admin.UpdateCharacter(ctx, adminID, uuid.MustParse(harukaID), content.CharacterPatch{
			Persona: &persona, Status: &disabled, Voice: &content.Voice{Gender: content.GenderMale, Accent: content.AccentKansai, SpeakingRate: 0.9}})
		require.NoError(t, err)

		loader := content.NewSessionConfigLoader(l.cached)
		rec2, err := l.runtime.GetForAttach(ctx, sid)
		require.NoError(t, err)
		cfg, err := loader.Load(ctx, content.SessionRefs{TopicID: rec2.TopicID, ScenarioVersionID: rec2.ScenarioVersionID, PromptVersionID: rec2.PromptVersionID, ConfigSnapshot: rec2.ConfigSnapshot})
		require.NoError(t, err, "a session whose character was disabled still loads")
		require.NotContains(t, cfg.Snapshot.Character.Persona, "diganti admin", "the snapshot is the old one")
		require.JSONEq(t, before, string(rec2.ConfigSnapshot))

		l.clk.Advance(60 * time.Second)
		require.NoError(t, l.runtime.BeginEnding(ctx, sid, fence, session.EndUserEnded))
		_, err = l.runtime.Complete(ctx, sid, fence)
		require.NoError(t, err, "the session ends normally")

		again := l.create(authz, "edit-key-0002", "USER_FIRST", "300")
		require.Equal(t, 422, again.code, string(again.body))
		require.Equal(t, "CHARACTER_NOT_AVAILABLE", again.errCode(t))

		enable := content.StatusActive
		_, err = l.admin.UpdateCharacter(ctx, adminID, uuid.MustParse(harukaID), content.CharacterPatch{Status: &enable})
		require.NoError(t, err)
	})

	t.Run("opening fallback", func(t *testing.T) {
		_, authz := l.newUser()
		_, err := l.pool.Exec(ctx, `UPDATE prompt_versions SET status = 'RETIRED', retired_at = now() WHERE prompt_config_id = '01920000-000c-7000-8000-000000000002' AND status = 'PUBLISHED'`)
		require.NoError(t, err)
		l.cached.(content.Invalidator).Invalidate()

		sid := l.sessionID(l.create(authz, "opening-key-0001", "AI_FIRST", "300"))
		fence, _, err := l.runtime.ClaimRuntime(ctx, sid, []session.Status{session.StatusReserved})
		require.NoError(t, err)
		rec, err := l.runtime.MarkStarting(ctx, sid, fence)
		require.NoError(t, err, "a missing OPENING never fails the session")
		require.Equal(t, session.StatusStarting, rec.Status)
		require.Nil(t, rec.OpeningPromptVersionID)
		require.Equal(t, 1, l.count(`SELECT count(*) FROM sessions WHERE id = $1 AND opening_prompt_version_id IS NULL AND status = 'STARTING'`, sid))
		require.Contains(t, l.logs.String(), "session.opening_prompt_missing")
		_, err = l.runtime.FailStart(ctx, sid, fence, session.EndProviderFailure)
		require.NoError(t, err)

		_, err = l.pool.Exec(ctx, `UPDATE prompt_versions SET status = 'PUBLISHED', retired_at = NULL, published_at = now() WHERE prompt_config_id = '01920000-000c-7000-8000-000000000002'`)
		require.NoError(t, err)
		l.cached.(content.Invalidator).Invalidate()
	})

	t.Run("provider failure", func(t *testing.T) {
		userID, authz := l.newUser()
		sid := l.sessionID(l.create(authz, "fail-key-0001", "USER_FIRST", "600"))
		fence, _ := l.start(sid)
		l.clk.Advance(45 * time.Second)
		require.NoError(t, l.runtime.BeginEnding(ctx, sid, fence, session.EndProviderFailure))
		st, err := l.runtime.Fail(ctx, sid, fence, session.EndProviderFailure)
		require.NoError(t, err)
		require.Zero(t, st.ChargedSeconds, "under 60 s of a service fault is free")
		avail, reserved := l.available(userID)
		require.Equal(t, int64(1200), avail)
		require.Zero(t, reserved)
		require.Equal(t, 1, l.count(`SELECT count(*) FROM event_outbox WHERE aggregate_id = $1 AND payload->>'final_status' = 'FAILED'`, sid))

		// a session that fails while STARTING publishes nothing
		sid2 := l.sessionID(l.create(authz, "fail-key-0002", "USER_FIRST", "300"))
		fence2, _, err := l.runtime.ClaimRuntime(ctx, sid2, []session.Status{session.StatusReserved})
		require.NoError(t, err)
		_, err = l.runtime.MarkStarting(ctx, sid2, fence2)
		require.NoError(t, err)
		_, err = l.runtime.FailStart(ctx, sid2, fence2, session.EndServerError)
		require.NoError(t, err)
		require.Zero(t, l.count(`SELECT count(*) FROM event_outbox WHERE aggregate_id = $1`, sid2))
	})

	t.Run("double click over HTTP", func(t *testing.T) {
		userID, authz := l.newUser()
		var wg sync.WaitGroup
		results := make([]lcResp, 2)
		for i := range results {
			wg.Add(1)
			go func() { defer wg.Done(); results[i] = l.create(authz, "double-click-0001", "USER_FIRST", "600") }()
		}
		wg.Wait()
		codes := []int{results[0].code, results[1].code}
		require.ElementsMatch(t, []int{201, 200}, codes, string(results[0].body)+string(results[1].body))
		require.Equal(t, l.sessionID(results[0]), l.sessionID(results[1]))
		for _, r := range results {
			if r.code == 200 {
				require.Equal(t, "true", r.header.Get("Idempotent-Replayed"))
			}
		}
		require.Equal(t, 1, l.count(`SELECT count(*) FROM balance_ledger WHERE user_id = $1 AND entry_type = 'RESERVE'`, userID))

		other := l.create(authz, "double-click-0001", "USER_FIRST", "300")
		require.Equal(t, 409, other.code)
		require.Equal(t, "IDEMPOTENCY_KEY_REUSED", other.errCode(t))
		another := l.create(authz, "double-click-0002", "USER_FIRST", "600")
		require.Equal(t, 409, another.code)
		require.Equal(t, "SESSION_ALREADY_OPEN", another.errCode(t))
		none := l.call(http.MethodPost, "/api/v1/sessions", l.body("USER_FIRST", "600"), "Authorization", authz)
		require.Equal(t, 400, none.code)
		require.Equal(t, "IDEMPOTENCY_KEY_REQUIRED", none.errCode(t))

		cancel := l.call(http.MethodPost, "/api/v1/sessions/"+l.sessionID(results[0]).String()+"/cancel", "", "Authorization", authz)
		require.Equal(t, 200, cancel.code, string(cancel.body))
	})

	t.Run("pod dies and the sweeper settles once", func(t *testing.T) {
		_, authz := l.newUser()
		sid := l.sessionID(l.create(authz, "pod-dies-0001", "USER_FIRST", "900"))
		fence, _ := l.start(sid)

		_, err := l.sweeper.Tick(ctx) // Redis seen for the first time
		require.NoError(t, err)
		l.clk.Advance(50 * time.Second)
		rep, err := l.sweeper.Tick(ctx)
		require.NoError(t, err)
		require.Equal(t, 1, rep.Rule["b"].OK, "no heartbeat for 50 s")
		require.Equal(t, 1, l.count(`SELECT count(*) FROM event_outbox WHERE aggregate_id = $1 AND event_type = 'session.completed'`, sid))
		var status, reason string
		require.NoError(t, l.pool.QueryRow(ctx, `SELECT status, end_reason FROM sessions WHERE id = $1`, sid).Scan(&status, &reason))
		require.Equal(t, "FAILED", status)
		require.Equal(t, "SERVER_ERROR", reason)

		// the runtime that was only slow comes back: it is told the session is over, nothing is written twice
		st, err := l.runtime.Complete(ctx, sid, fence)
		if err != nil {
			require.ErrorIs(t, err, session.ErrFenceLost)
		} else {
			require.Equal(t, session.EndServerError, st.EndReason)
		}
		require.Equal(t, 1, l.count(`SELECT count(*) FROM event_outbox WHERE aggregate_id = $1 AND event_type = 'session.completed'`, sid))
		rep, err = l.sweeper.Tick(ctx)
		require.NoError(t, err)
		require.Zero(t, rep.Rule["b"].OK, "the next tick finds nothing")
	})

	t.Run("abandoned reservation", func(t *testing.T) {
		userID, authz := l.newUser()
		sid := l.sessionID(l.create(authz, "abandon-key-0001", "USER_FIRST", "600"))
		avail, reserved := l.available(userID)
		require.Equal(t, int64(600), avail)
		require.Equal(t, int64(600), reserved)

		before, err := l.svc.Abandon(ctx, sid, session.ActorCleanup)
		require.NoError(t, err)
		require.Equal(t, session.StatusReserved, before.Status, "the deadline has not passed")
		l.clk.Advance(5*time.Minute + time.Second)
		got, err := l.svc.Abandon(ctx, sid, session.ActorCleanup)
		require.NoError(t, err)
		require.Equal(t, session.StatusAbandoned, got.Status)
		avail, reserved = l.available(userID)
		require.Equal(t, int64(1200), avail)
		require.Zero(t, reserved)
	})

	t.Run("transcript access", func(t *testing.T) {
		_, authz := l.newUser()
		sid := l.sessionID(l.create(authz, "transcript-key-0001", "USER_FIRST", "300"))
		fence, _ := l.start(sid)
		turn := session.TurnStart{ID: uuid.Must(uuid.NewV7()), SessionID: sid, Sequence: 1, Generation: 1, CreatedAt: l.clk.Now()}
		require.NoError(t, l.store.SessionStore().WithinTx(ctx, func(ctx context.Context, tx session.Tx) error {
			if err := tx.Turns().Insert(ctx, fence, turn); err != nil {
				return err
			}
			text, reply := "こんにちは", "はじめまして"
			if err := tx.Turns().UpdateUser(ctx, fence, turn.ID, session.TurnUser{CreatedAt: turn.CreatedAt, Text: text}); err != nil {
				return err
			}
			return tx.Turns().Finalize(ctx, fence, turn.ID, session.TurnFinal{CreatedAt: turn.CreatedAt, Status: session.TurnCompleted, AssistantText: &reply, CompletedAt: l.clk.Now()})
		}))
		r := l.call(http.MethodGet, "/api/v1/sessions/"+sid.String()+"/turns", "", "Authorization", authz)
		require.Equal(t, 200, r.code, string(r.body))
		require.Len(t, r.json(t)["data"], 1, "an ACTIVE session's transcript is readable")

		l.clk.Advance(30 * time.Second)
		require.NoError(t, l.runtime.BeginEnding(ctx, sid, fence, session.EndUserEnded))
		_, err := l.runtime.Complete(ctx, sid, fence)
		require.NoError(t, err)

		l.clk.Advance(181 * 24 * time.Hour)
		login := l.call(http.MethodPost, "/api/v1/auth/login", `{"email":"user`+string(rune('a'+l.users))+`@example.com","password":"correct horse battery"}`)
		require.Equal(t, 200, login.code, string(login.body))
		fresh := "Bearer " + login.json(t)["access_token"].(string)
		r = l.call(http.MethodGet, "/api/v1/sessions/"+sid.String()+"/turns", "", "Authorization", fresh)
		require.Equal(t, 410, r.code, string(r.body))
		require.Equal(t, "TRANSCRIPT_EXPIRED", r.errCode(t))
	})

	t.Run("no orphan reservation after all scenarios", func(t *testing.T) {
		testutil.AssertReconciled(t, l.pool)
		violations, err := balance.NewService(l.store.BalanceStore(), l.clk, idgen.NewV7()).Reconcile(ctx, postgres.NewReconcileRepo(l.pool))
		require.NoError(t, err)
		require.Empty(t, violations, "R1–R6")
		require.Zero(t, l.count(`SELECT count(*) FROM balance_reservations r JOIN sessions s ON s.id = r.session_id
			WHERE r.status = 'HELD' AND s.status IN ('COMPLETED','FAILED','CANCELLED','ABANDONED')`), "no HELD reservation of a finished session")
		require.Zero(t, l.count(`SELECT count(*) FROM sessions s WHERE s.status IN ('COMPLETED','FAILED','CANCELLED','ABANDONED') AND NOT EXISTS (SELECT 1 FROM balance_reservations r WHERE r.session_id = s.id AND r.status IN ('SETTLED','RELEASED'))`),
			"every finished session has a settled or released reservation")
	})
}
