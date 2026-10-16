//go:build integration

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/auth"
	"github.com/nabePi/kotoki-backend/internal/balance"
	"github.com/nabePi/kotoki-backend/internal/learner"
	"github.com/nabePi/kotoki-backend/internal/platform/clock"
	"github.com/nabePi/kotoki-backend/internal/platform/config"
	"github.com/nabePi/kotoki-backend/internal/platform/idgen"
	"github.com/nabePi/kotoki-backend/internal/postgres"
	"github.com/nabePi/kotoki-backend/internal/testutil"
)

// apiRig is the real buildAPI behind an httptest server, over Postgres and Redis containers.
type apiRig struct {
	t       *testing.T
	srv     *httptest.Server
	pool    *pgxpool.Pool
	content testutil.ContentIDs
	tokens  auth.TokenIssuer
}

func newAPIRig(t *testing.T) *apiRig {
	t.Helper()
	pool := testutil.NewPostgres(t)
	rdb := testutil.NewRedis(t)
	testutil.ApplyEnv(t, testutil.MinimalEnv(t, map[string]string{"PROVIDER_MODE": "fake"}))
	cfg, err := config.LoadAPI()
	require.NoError(t, err)
	secret, err := cfg.JWTSecretBytes()
	require.NoError(t, err)
	tokens, err := auth.NewJWTIssuer(auth.JWTConfig{
		Secret: secret, Issuer: cfg.JWTIssuer, Audience: cfg.JWTAudience, TTL: time.Hour,
	}, idgen.NewV7())
	require.NoError(t, err)
	seeded := testutil.SeedContent(t, pool)
	router, dispatcher, err := buildAPI(context.Background(), cfg, slog.New(slog.DiscardHandler), pool, rdb, prometheus.NewRegistry())
	require.NoError(t, err)
	t.Cleanup(func() { _ = dispatcher.Close(context.Background()) })
	srv := httptest.NewServer(router)
	t.Cleanup(srv.Close)
	return &apiRig{t: t, srv: srv, pool: pool, content: seeded, tokens: tokens}
}

// user seeds an ACTIVE user who accepted the current terms, with a funded account.
func (r *apiRig) user() uuid.UUID {
	r.t.Helper()
	uid := testutil.SeedUser(r.t, r.pool, testutil.Row{"terms_version": "2026-10-01"})
	ids, clk := idgen.NewV7(), clock.System{}
	store := postgres.NewStore(r.pool, postgres.WithIDs(ids), postgres.WithClock(clk))
	require.NoError(r.t, store.BalanceStore().WithinTx(context.Background(), func(ctx context.Context, tx balance.Tx) error {
		return balance.OpenAccount(ctx, tx, ids, uid, 3600, clk.Now())
	}))
	return uid
}

func (r *apiRig) call(uid uuid.UUID, method, path, body string, headers ...string) (int, []byte) {
	r.t.Helper()
	tok, _, err := r.tokens.IssueAccess(auth.AccessSubject{UserID: uid, Role: auth.RoleUser, FamilyID: uuid.Must(uuid.NewV7())}, time.Now())
	require.NoError(r.t, err)
	var rd io.Reader
	if body != "" {
		rd = bytes.NewReader([]byte(body))
	}
	req, err := http.NewRequestWithContext(context.Background(), method, r.srv.URL+path, rd)
	require.NoError(r.t, err)
	req.Header.Set("Authorization", "Bearer "+tok)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	res, err := http.DefaultClient.Do(req)
	require.NoError(r.t, err)
	defer func() { _ = res.Body.Close() }()
	b, err := io.ReadAll(res.Body)
	require.NoError(r.t, err)
	return res.StatusCode, b
}

func (r *apiRig) createSession(uid uuid.UUID, key string) (int, []byte) {
	r.t.Helper()
	body, err := json.Marshal(map[string]any{
		"topic_id": r.content.TopicID, "scenario_id": r.content.ScenarioID, "character_id": r.content.CharacterID,
		"duration_seconds": 300, "conversation_start_mode": "USER_FIRST",
	})
	require.NoError(r.t, err)
	return r.call(uid, http.MethodPost, "/api/v1/sessions", string(body), "Idempotency-Key", key)
}

func TestPostSessions_StoresLearnerAdaptiveSnapshot(t *testing.T) {
	r := newAPIRig(t)
	uid := r.user()

	// the profile endpoints are mounted on the real router: onboarding sets the N4 priors
	code, body := r.call(uid, http.MethodPatch, "/api/v1/profile", `{"self_declared_level":"N4"}`)
	require.Equal(t, http.StatusOK, code, string(body))

	code, body = r.createSession(uid, "m8-wiring-"+uuid.NewString()[:8])
	require.Equal(t, http.StatusCreated, code, string(body))
	var created struct {
		ID uuid.UUID `json:"id"`
	}
	require.NoError(t, json.Unmarshal(body, &created))

	var adRaw, snapRaw []byte
	var difficulty int
	require.NoError(t, r.pool.QueryRow(context.Background(), `
		SELECT s.adaptive_difficulty, s.config_snapshot, sv.difficulty_level
		FROM sessions s JOIN scenario_versions sv ON sv.id = s.scenario_version_id WHERE s.id = $1`, created.ID).Scan(&adRaw, &snapRaw, &difficulty))
	require.NotEmpty(t, snapRaw, "config_snapshot is stored")

	var ad learner.AdaptiveDifficulty
	require.NoError(t, json.Unmarshal(adRaw, &ad), string(adRaw))
	require.Equal(t, 1, ad.AlgorithmVersion)
	require.Equal(t, difficulty, ad.ScenarioDifficulty, "difficulty_level of the scenario version")
	require.Equal(t, learner.FormalityPolite, ad.Formality, "scenario formality POLITE, no preference")
	// a neutral provider would store the same dials for every user; N4 priors (receptive 2.00,
	// productive 1.50) put the stretch above the 1.00 floor of a user without onboarding
	other := r.user()
	code, body = r.createSession(other, "m8-wiring-"+uuid.NewString()[:8])
	require.Equal(t, http.StatusCreated, code, string(body))
	var otherCreated struct {
		ID uuid.UUID `json:"id"`
	}
	require.NoError(t, json.Unmarshal(body, &otherCreated))
	var otherRaw []byte
	require.NoError(t, r.pool.QueryRow(context.Background(), `SELECT adaptive_difficulty FROM sessions WHERE id = $1`, otherCreated.ID).Scan(&otherRaw))
	var otherAD learner.AdaptiveDifficulty
	require.NoError(t, json.Unmarshal(otherRaw, &otherAD))
	require.Greater(t, ad.GrammarComplexity, otherAD.GrammarComplexity, "the N4 user is given a harder grammar level than a user with no onboarding")
}

func TestPostSessions_LearnerFailureIs503AndLeavesNothing(t *testing.T) {
	r := newAPIRig(t)
	uid := r.user()
	_, err := r.pool.Exec(context.Background(), `ALTER TABLE learner_profiles RENAME TO learner_profiles_gone`)
	require.NoError(t, err)

	code, body := r.createSession(uid, "m8-wiring-down")
	require.Equal(t, http.StatusServiceUnavailable, code, string(body))
	require.True(t, strings.Contains(string(body), "SERVICE_UNAVAILABLE"), string(body))

	var sessions, reservations int
	require.NoError(t, r.pool.QueryRow(context.Background(), `SELECT count(*) FROM sessions WHERE user_id = $1`, uid).Scan(&sessions))
	require.NoError(t, r.pool.QueryRow(context.Background(), `SELECT count(*) FROM balance_ledger WHERE user_id = $1 AND entry_type = 'RESERVE'`, uid).Scan(&reservations))
	require.Zero(t, sessions, "no session")
	require.Zero(t, reservations, "no reservation")
}
