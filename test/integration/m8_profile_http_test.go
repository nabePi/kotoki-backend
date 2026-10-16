//go:build integration

package integration_test

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
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/auth"
	"github.com/nabePi/kotoki-backend/internal/httpapi"
	"github.com/nabePi/kotoki-backend/internal/learner"
	"github.com/nabePi/kotoki-backend/internal/memory"
	"github.com/nabePi/kotoki-backend/internal/platform/clock"
	"github.com/nabePi/kotoki-backend/internal/platform/idgen"
	"github.com/nabePi/kotoki-backend/internal/postgres"
	"github.com/nabePi/kotoki-backend/internal/testutil"
)

const (
	profileBase  = "/api/v1/profile"
	memoriesBase = "/api/v1/profile/memories"
	m8Terms      = "2026-10-01"
)

// profileHTTP is the real router over Postgres and Redis with the real learner and memory
// services; users are seeded rows with a token issued for them (the endpoints trust the token).
type profileHTTP struct {
	*retrievalRig
	srv    *httptest.Server
	tokens *auth.JWTIssuer
	store  *postgres.Store
}

type httpResp struct {
	code int
	body []byte
}

func (r httpResp) json(t *testing.T) map[string]any {
	t.Helper()
	var m map[string]any
	require.NoError(t, json.Unmarshal(r.body, &m), string(r.body))
	return m
}

func (r httpResp) errCode(t *testing.T) string {
	t.Helper()
	e, _ := r.json(t)["error"].(map[string]any)
	c, _ := e["code"].(string)
	return c
}

func newProfileHTTP(t *testing.T) *profileHTTP {
	t.Helper()
	rr, _ := newRetrievalRigPG(t)
	ids := idgen.NewV7()
	clk := clock.System{}
	store := postgres.NewStore(rr.rig.pool, postgres.WithIDs(ids), postgres.WithClock(clk))
	tokens, err := auth.NewJWTIssuer(auth.JWTConfig{Secret: []byte(strings.Repeat("m", 32)), Issuer: "kotoki-api", Audience: "kotoki", TTL: time.Hour}, ids)
	require.NoError(t, err)
	hasher := auth.NewArgon2Hasher(auth.Argon2Config{MemoryKiB: 64, Iterations: 1, Parallelism: 1, MaxConcurrent: 2, MaxWait: time.Second})
	users := auth.NewUserService(store.AuthStore(), hasher, nil, clk, nil, m8Terms)
	profile := learner.NewService(learner.Deps{Store: store.LearnerStore(), Clock: clk, IDs: ids})
	mem := memory.NewService(store.MemoryStore(), rr.bump, postgres.NewMemoryProfileSettings(rr.rig.pool, ids, clk), clk, nil)

	router := httpapi.NewRouter(httpapi.Deps{
		Log: slog.New(slog.DiscardHandler), Clock: clk, Tokens: tokens, Limiter: allowAll{}, Users: users, Profile: profile, Memories: mem,
		Cfg: httpapi.Config{BodyLimit: 64 << 10},
	})
	srv := httptest.NewServer(router)
	t.Cleanup(srv.Close)
	return &profileHTTP{retrievalRig: rr, srv: srv, tokens: tokens, store: store}
}

func (h *profileHTTP) bearer(uid uuid.UUID) string {
	h.t.Helper()
	tok, _, err := h.tokens.IssueAccess(auth.AccessSubject{UserID: uid, Role: auth.RoleUser, FamilyID: uuid.Must(uuid.NewV7())}, time.Now())
	require.NoError(h.t, err)
	return "Bearer " + tok
}

func (h *profileHTTP) call(uid uuid.UUID, method, path, body string) httpResp {
	h.t.Helper()
	var rd io.Reader
	if body != "" {
		rd = bytes.NewReader([]byte(body))
	}
	req, err := http.NewRequestWithContext(context.Background(), method, h.srv.URL+path, rd)
	require.NoError(h.t, err)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", h.bearer(uid))
	res, err := http.DefaultClient.Do(req)
	require.NoError(h.t, err)
	defer func() { _ = res.Body.Close() }()
	b, err := io.ReadAll(res.Body)
	require.NoError(h.t, err)
	return httpResp{code: res.StatusCode, body: b}
}

func (h *profileHTTP) memCount(uid uuid.UUID, where string) int {
	return h.rig.memCount(uid, where)
}

func (h *profileHTTP) memoryID(uid uuid.UUID, key string) uuid.UUID {
	h.t.Helper()
	var id uuid.UUID
	require.NoError(h.t, h.rig.pool.QueryRow(context.Background(),
		`SELECT id FROM memories WHERE user_id = $1 AND memory_key = $2 AND status = 'ACTIVE'`, uid, key).Scan(&id))
	return id
}

func (h *profileHTTP) memver(uid uuid.UUID) int64 {
	h.t.Helper()
	v, err := h.cache.GetMemVer(context.Background(), uid)
	require.NoError(h.t, err)
	return v
}

func listedIDs(t *testing.T, r httpResp) []string {
	t.Helper()
	require.Equal(t, 200, r.code, string(r.body))
	var out []string
	for _, it := range r.json(t)["data"].([]any) {
		out = append(out, it.(map[string]any)["id"].(string))
	}
	return out
}

func activeIDs(t *testing.T, h *profileHTTP, uid uuid.UUID) []string {
	t.Helper()
	rows, err := h.rig.pool.Query(context.Background(), `SELECT id::text FROM memories WHERE user_id = $1 AND status = 'ACTIVE' ORDER BY id`, uid)
	require.NoError(t, err)
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		require.NoError(t, rows.Scan(&s))
		out = append(out, s)
	}
	return out
}

func TestListMemories_OnlyOwn(t *testing.T) {
	h := newProfileHTTP(t)
	a := h.extract(uuid.Nil)
	b := h.extract(uuid.Nil) // identical transcripts: identical content and embeddings

	got := listedIDs(t, h.call(a.UserID, http.MethodGet, memoriesBase+"?limit=100", ""))
	require.ElementsMatch(t, activeIDs(t, h, a.UserID), got)
	require.Len(t, got, 4)
	other := listedIDs(t, h.call(b.UserID, http.MethodGet, memoriesBase, ""))
	require.ElementsMatch(t, activeIDs(t, h, b.UserID), other)
	for _, id := range other {
		require.NotContains(t, got, id, "no memory of B in the list of A")
	}

	// the user wire shape: content in Japanese, display_text in Indonesian, no embedding or key
	body := h.call(a.UserID, http.MethodGet, memoriesBase, "").json(t)
	require.Equal(t, true, body["memory_enabled"])
	item := body["data"].([]any)[0].(map[string]any)
	for _, banned := range []string{"embedding", "memory_key", "status", "user_id"} {
		require.NotContains(t, item, banned)
	}
}

func TestListMemories_PagingOnPostgres(t *testing.T) {
	h := newProfileHTTP(t)
	a := h.extract(uuid.Nil)
	want := activeIDs(t, h, a.UserID)
	var got []string
	path := memoriesBase + "?limit=3"
	for pages := 0; path != ""; pages++ {
		require.Less(t, pages, 5)
		r := h.call(a.UserID, http.MethodGet, path, "")
		got = append(got, listedIDs(t, r)...)
		path = ""
		if c, ok := r.json(t)["next_cursor"].(string); ok {
			path = memoriesBase + "?limit=3&cursor=" + c
		}
	}
	require.ElementsMatch(t, want, got, "every memory exactly once")
	require.Len(t, got, 4)

	ep := h.call(a.UserID, http.MethodGet, memoriesBase+"?category=EPISODIC", "")
	require.Len(t, listedIDs(t, ep), 1)
}

func TestDeleteMemory_OtherUser404(t *testing.T) {
	h := newProfileHTTP(t)
	a := h.extract(uuid.Nil)
	b := h.extract(uuid.Nil)
	theirs := h.memoryID(b.UserID, "occupation")

	r := h.call(a.UserID, http.MethodDelete, memoriesBase+"/"+theirs.String(), "")
	require.Equal(t, 404, r.code, string(r.body))
	require.Equal(t, "MEMORY_NOT_FOUND", r.errCode(t))
	require.Equal(t, 4, h.memCount(b.UserID, `AND status = 'ACTIVE'`), "the rows of B stay")
	require.Equal(t, 0, h.memCount(b.UserID, `AND status = 'DELETED'`), "no tombstone for B")
	require.Equal(t, 4, h.memCount(a.UserID, `AND status = 'ACTIVE'`))
	require.Zero(t, h.memver(b.UserID))
	require.Zero(t, h.memver(a.UserID))

	// an id that does not exist at all answers identically
	miss := h.call(a.UserID, http.MethodDelete, memoriesBase+"/"+uuid.NewString(), "")
	require.Equal(t, 404, miss.code)
	require.Equal(t, "MEMORY_NOT_FOUND", miss.errCode(t))
}

func TestDeleteMemory_Owner204ThenSlotTombstoned(t *testing.T) {
	h := newProfileHTTP(t)
	first := h.extract(uuid.Nil)
	uid := first.UserID
	id := h.memoryID(uid, "occupation")

	// a session is running with the memory loaded
	mc, sid := h.build(uid, "面接練習")
	require.NotEmpty(t, mc.Core)
	var loaded bool
	for _, it := range mc.Core {
		loaded = loaded || it.ID == id
	}
	require.True(t, loaded)

	r := h.call(uid, http.MethodDelete, memoriesBase+"/"+id.String(), "")
	require.Equal(t, 204, r.code, string(r.body))
	require.Empty(t, r.body)
	require.Equal(t, 0, h.memCount(uid, `AND memory_key = 'occupation' AND status = 'ACTIVE'`))
	require.Equal(t, 1, h.memCount(uid, `AND memory_key = 'occupation' AND status = 'DELETED' AND content = ''`), "tombstone, no content")
	require.Equal(t, int64(1), h.memver(uid))
	require.NotContains(t, listedIDs(t, h.call(uid, http.MethodGet, memoriesBase, "")), id.String())

	// the running session drops it on its next reload
	mc.SessionID = sid
	changed, err := h.ret.ReloadCoreIfChanged(context.Background(), &mc)
	require.NoError(t, err)
	require.True(t, changed)
	for _, it := range mc.Core {
		require.NotEqual(t, id, it.ID)
	}

	// the next session's extraction does not recreate the slot (Review Focus 4)
	second := h.rig.session(testutil.FinishedSessionOpts{UserID: uid})
	require.NoError(t, h.rig.handle(second.Envelope))
	require.Equal(t, 0, h.memCount(uid, `AND memory_key = 'occupation' AND status = 'ACTIVE'`))
	require.Equal(t, 1, h.memCount(uid, `AND memory_key = 'occupation' AND status = 'DELETED'`))

	// the same id again: gone
	again := h.call(uid, http.MethodDelete, memoriesBase+"/"+id.String(), "")
	require.Equal(t, 404, again.code)
	require.Equal(t, "MEMORY_NOT_FOUND", again.errCode(t))
}

func TestDeleteAllMemories_ThenListEmpty(t *testing.T) {
	h := newProfileHTTP(t)
	a := h.extract(uuid.Nil)
	b := h.extract(uuid.Nil)
	uid := a.UserID
	// a superseded episode (second session) and a tombstone
	h.extract(uid)
	require.Equal(t, 1, h.memCount(uid, `AND status = 'SUPERSEDED'`))
	require.Equal(t, 204, h.call(uid, http.MethodDelete, memoriesBase+"/"+h.memoryID(uid, "occupation").String(), "").code)
	require.Equal(t, 1, h.memCount(uid, `AND status = 'DELETED'`))
	before := h.memver(uid)

	r := h.call(uid, http.MethodDelete, memoriesBase, "")
	require.Equal(t, 204, r.code, string(r.body))
	require.Equal(t, 0, h.memCount(uid, ``), "every row: active, superseded and tombstone")
	require.Equal(t, before+1, h.memver(uid))
	require.Empty(t, listedIDs(t, h.call(uid, http.MethodGet, memoriesBase, "")))
	mc, _ := h.build(uid, "面接練習")
	require.Empty(t, mc.Core)
	require.Empty(t, mc.Episodic)

	require.Equal(t, 204, h.call(uid, http.MethodDelete, memoriesBase, "").code, "idempotent")
	require.Equal(t, 4, h.memCount(b.UserID, `AND status = 'ACTIVE'`), "the memories of B stay")
	require.Zero(t, h.memver(b.UserID))
}

func TestPatchMemoryEnabledFalse_WorkerSkipsAndRetrievalEmpty(t *testing.T) {
	h := newProfileHTTP(t)
	first := h.extract(uuid.Nil)
	uid := first.UserID
	require.Equal(t, 4, h.memCount(uid, `AND status = 'ACTIVE'`))
	llm := h.rig.prov.llmCalls.Load()

	r := h.call(uid, http.MethodPatch, profileBase, `{"preferences":{"memory_enabled":false}}`)
	require.Equal(t, 200, r.code, string(r.body))
	require.Equal(t, false, r.json(t)["preferences"].(map[string]any)["memory_enabled"])

	// the worker skips the next session
	second := h.rig.session(testutil.FinishedSessionOpts{UserID: uid})
	require.NoError(t, h.rig.handle(second.Envelope))
	require.Equal(t, llm, h.rig.prov.llmCalls.Load(), "no extraction call")
	require.Equal(t, 4, h.memCount(uid, `AND status = 'ACTIVE'`))
	require.Equal(t, 0, h.memCount(uid, `AND observation_count > 1`))

	// retrieval is empty and not degraded; the stored memories are kept and still listed
	mc, _ := h.build(uid, "面接練習（バックエンドエンジニア）")
	require.Empty(t, mc.Core)
	require.Empty(t, mc.Episodic)
	require.False(t, mc.Degraded)
	l := h.call(uid, http.MethodGet, memoriesBase, "")
	require.Len(t, listedIDs(t, l), 4)
	require.Equal(t, false, l.json(t)["memory_enabled"])

	// switching it on again brings both back
	require.Equal(t, 200, h.call(uid, http.MethodPatch, profileBase, `{"preferences":{"memory_enabled":true}}`).code)
	mc, _ = h.build(uid, "面接練習（バックエンドエンジニア）")
	require.NotEmpty(t, mc.Core)
}

func TestWriteEndpoints_SuspendedUserOnPostgres(t *testing.T) {
	h := newProfileHTTP(t)
	fs := h.rig.session(testutil.FinishedSessionOpts{UserStatus: "SUSPENDED"})
	require.NoError(t, h.rig.handle(fs.Envelope))
	uid := fs.UserID
	id := h.memoryID(uid, "occupation")

	for _, c := range []struct{ method, path, body string }{
		{http.MethodPatch, profileBase, `{"nickname":"x"}`},
		{http.MethodDelete, profileBase + "/skills", ""},
		{http.MethodDelete, memoriesBase, ""},
		{http.MethodDelete, memoriesBase + "/" + id.String(), ""},
	} {
		r := h.call(uid, c.method, c.path, c.body)
		require.Equal(t, 403, r.code, c.method+" "+c.path+" "+string(r.body))
		require.Equal(t, "ACCOUNT_SUSPENDED", r.errCode(t))
	}
	require.Equal(t, 4, h.memCount(uid, `AND status = 'ACTIVE'`))
	require.Equal(t, 200, h.call(uid, http.MethodGet, memoriesBase, "").code)
	require.Equal(t, 200, h.call(uid, http.MethodGet, profileBase, "").code)
}

// seedEstimate gives the user a COMPLETED session with an evaluation and one GRAMMAR estimate, and
// the skill state that follows from it.
func (h *profileHTTP) seedEstimate(uid uuid.UUID) uuid.UUID {
	h.t.Helper()
	ctx := context.Background()
	fs := h.rig.session(testutil.FinishedSessionOpts{UserID: uid})
	now := time.Now().UTC().Truncate(time.Microsecond)
	evID := uuid.Must(uuid.NewV7())
	testutil.MustInsert(ctx, h.t, h.rig.pool, "evaluations", testutil.Row{
		"id": evID, "session_id": fs.ID, "user_id": uid, "status": "COMPLETED", "rubric_version": "rubric-v1",
		"source_event_id": uuid.Must(uuid.NewV7()), "grammar_score": 60, "vocabulary_score": 61, "fluency_score": 62,
		"naturalness_score": 63, "task_achievement_score": 64, "overall_score": 62.0, "completed_at": now, "started_at": now,
	})
	d := 3
	require.NoError(h.t, h.store.LearnerStore().WithinTx(ctx, func(ctx context.Context, tx learner.Tx) error {
		if err := tx.Profiles().Ensure(ctx, uid); err != nil {
			return err
		}
		if _, err := tx.Estimates().Insert(ctx, uid, learner.EstimateRow{
			ID: uuid.Must(uuid.NewV7()), EvaluationID: evID, SessionID: fs.ID, Skill: learner.SkillGrammar, AlgorithmVersion: 1,
			RawScore: 60, DEff: &d, LObs: 2.1, CObs: 0.5, AssessedAt: now, Previous: 1.0, Level: 1.4, Confidence: 0.3, Count: 1, Trend: learner.TrendInsufficient,
		}); err != nil {
			return err
		}
		return tx.SkillStates().Save(ctx, uid, learner.StateRow{
			Skill: learner.SkillGrammar, PriorLevel: 1.0, PriorConfidence: 0.05, Level: 1.4, Confidence: 0.3, Trend: learner.TrendInsufficient,
			EvidenceCount: 1, LastEvaluationID: &evID, LastEvidenceAt: &now,
		}, now)
	}))
	return evID
}

func TestProfileEndpoints_OnlyOwnData(t *testing.T) {
	h := newProfileHTTP(t)
	a := testutil.SeedUser(t, h.rig.pool, nil)
	b := testutil.SeedUser(t, h.rig.pool, nil)
	evB := h.seedEstimate(b)

	require.Equal(t, 200, h.call(b, http.MethodPatch, profileBase, `{"nickname":"Bima","self_declared_level":"N3"}`).code)

	// B sees its estimate
	hb := h.call(b, http.MethodGet, profileBase+"/skills/GRAMMAR/history", "").json(t)
	require.Len(t, hb["data"], 1)
	require.Equal(t, evB.String(), hb["data"].([]any)[0].(map[string]any)["evaluation_id"])
	sk := h.call(b, http.MethodGet, profileBase+"/skills", "").json(t)["skills"].([]any)[0].(map[string]any)
	require.Equal(t, float64(1), sk["evidence_count"])

	// A sees none of it, nor of B's profile
	ha := h.call(a, http.MethodGet, profileBase+"/skills/GRAMMAR/history", "").json(t)
	require.Empty(t, ha["data"])
	for _, s := range h.call(a, http.MethodGet, profileBase+"/skills", "").json(t)["skills"].([]any) {
		require.Zero(t, s.(map[string]any)["evidence_count"])
	}
	pa := h.call(a, http.MethodGet, profileBase, "").json(t)
	require.Equal(t, a.String(), pa["user_id"])
	require.Nil(t, pa["nickname"])
	require.Nil(t, pa["self_declared_level"])
	require.Zero(t, pa["completed_session_count"])
	pb := h.call(b, http.MethodGet, profileBase, "").json(t)
	require.Equal(t, "Bima", pb["nickname"])
	require.Equal(t, float64(1), pb["completed_session_count"], "stats are of the caller's sessions only")
}

func TestDeleteSkills_ResetsOnlyOwn(t *testing.T) {
	h := newProfileHTTP(t)
	a := testutil.SeedUser(t, h.rig.pool, nil)
	b := testutil.SeedUser(t, h.rig.pool, nil)
	require.Equal(t, 200, h.call(a, http.MethodPatch, profileBase, `{"nickname":"Ayu","self_declared_level":"N4"}`).code) // priors, before any evidence
	h.seedEstimate(a)
	h.seedEstimate(b)

	for i := 0; i < 2; i++ { // idempotent
		r := h.call(a, http.MethodDelete, profileBase+"/skills", "")
		require.Equal(t, 204, r.code, string(r.body))
		require.Empty(t, r.body)
	}
	require.Equal(t, 0, count(t, h.rig.pool, `SELECT count(*) FROM proficiency_estimates WHERE user_id = $1`, a))
	require.Equal(t, 1, count(t, h.rig.pool, `SELECT count(*) FROM proficiency_estimates WHERE user_id = $1`, b), "B keeps its history")
	require.Equal(t, 1, count(t, h.rig.pool, `SELECT count(*) FROM learner_skill_states WHERE user_id = $1 AND evidence_count = 1`, b))
	require.Equal(t, 0, count(t, h.rig.pool, `SELECT count(*) FROM learner_skill_states WHERE user_id = $1 AND evidence_count <> 0`, a))
	require.Equal(t, 2, count(t, h.rig.pool, `SELECT count(*) FROM evaluations WHERE user_id IN ($1, $2)`, a, b), "evaluations untouched")

	pa := h.call(a, http.MethodGet, profileBase, "").json(t)
	require.Equal(t, "Ayu", pa["nickname"], "the profile survives the reset")
	sk := h.call(a, http.MethodGet, profileBase+"/skills", "").json(t)["skills"].([]any)[0].(map[string]any)
	require.Equal(t, float64(2), sk["estimated_level"], "back to the N4 prior")
	require.Equal(t, "INSUFFICIENT_DATA", sk["trend"])
}
