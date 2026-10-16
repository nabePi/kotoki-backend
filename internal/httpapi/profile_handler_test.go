package httpapi_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/auth"
	"github.com/nabePi/kotoki-backend/internal/learner"
)

const profilePath = "/api/v1/profile"

// profileUser registers a user and returns the id and the bearer header.
func (e *apiEnv) profileUser(t *testing.T, email string) (uuid.UUID, string) {
	t.Helper()
	authz := bearerOf(t, e.register(t, email))
	return e.userByEmail(t, email).ID, authz
}

func (e *apiEnv) suspend(id uuid.UUID) {
	e.store.MutateUser(id, func(u *auth.User) { u.Status = auth.UserStatusSuspended })
}

func jsonOf(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &m), rec.Body.String())
	return m
}

func TestGetProfileGolden(t *testing.T) {
	e := newAPI(t)
	uid, authz := e.profileUser(t, "hana@example.com")
	rec := e.do(http.MethodPatch, profilePath, `{"nickname":"Hana","self_declared_level":"N4","target_level":"N2","learning_goal":"JOB_INTERVIEW",
		"learning_goal_note":"Wawancara kerja di Tokyo tahun depan","preferences":{"speech_speed":"SLOWER"}}`, "Authorization", authz)
	require.Equal(t, 200, rec.Code, rec.Body.String())
	last := time.Date(2026, 10, 8, 11, 30, 0, 123_000_000, time.UTC)
	e.learnerStore.SetStats(uid, learner.ProfileStats{CompletedSessions: 12, PracticeSeconds: 7230, LastSessionAt: &last})

	rec = e.do(http.MethodGet, profilePath, "", "Authorization", authz)
	require.Equal(t, 200, rec.Code, rec.Body.String())
	goldenFile(t, "profile", "get_profile.json", rec.Body.Bytes())

	m := jsonOf(t, rec)
	require.Len(t, m, 15)
	require.Equal(t, uid.String(), m["user_id"])
	require.Equal(t, "2026-10-08T11:30:00.123Z", m["last_session_at"])
	require.Equal(t, float64(12), m["completed_session_count"])
	require.Equal(t, float64(7230), m["total_practice_seconds"])
	require.NotContains(t, rec.Body.String(), "Tanaka", "users.full_name never reaches the profile")
}

func TestGetProfile_NewUserHasNullFields(t *testing.T) {
	e := newAPI(t)
	_, authz := e.profileUser(t, "hana@example.com")
	rec := e.do(http.MethodGet, profilePath, "", "Authorization", authz)
	require.Equal(t, 200, rec.Code, rec.Body.String())
	goldenFile(t, "profile", "get_profile_new.json", rec.Body.Bytes())
	m := jsonOf(t, rec)
	for _, k := range []string{"nickname", "self_declared_level", "target_level", "learning_goal", "learning_goal_note", "last_session_at", "onboarding_completed_at"} {
		v, ok := m[k]
		require.True(t, ok, k+" is always present")
		require.Nil(t, v, k)
	}
}

func TestPatchProfileGolden(t *testing.T) {
	e := newAPI(t)
	_, authz := e.profileUser(t, "hana@example.com")
	rec := e.do(http.MethodPatch, profilePath, `{"nickname":" Hana ","self_declared_level":"N3","preferences":{"speech_speed":"FASTER","formality":"POLITE","dialect":"ALLOW_DIALECT","memory_enabled":false}}`,
		"Authorization", authz)
	require.Equal(t, 200, rec.Code, rec.Body.String())
	goldenFile(t, "profile", "patch_profile_200.json", rec.Body.Bytes())

	m := jsonOf(t, rec)
	require.Equal(t, "Hana", m["nickname"])
	require.Equal(t, map[string]any{"speech_speed": "FASTER", "formality": "POLITE", "dialect": "ALLOW_DIALECT", "memory_enabled": false}, m["preferences"])
	require.NotNil(t, m["onboarding_completed_at"])

	// a later GET shows the same state
	got := e.do(http.MethodGet, profilePath, "", "Authorization", authz)
	require.Equal(t, rec.Body.String(), got.Body.String())
}

func TestPatchProfile_NullClearsNickname(t *testing.T) {
	e := newAPI(t)
	_, authz := e.profileUser(t, "hana@example.com")
	require.Equal(t, 200, e.do(http.MethodPatch, profilePath, `{"nickname":"Hana","learning_goal":"TRAVEL","learning_goal_note":"x"}`, "Authorization", authz).Code)
	rec := e.do(http.MethodPatch, profilePath, `{"nickname":null,"learning_goal_note":null}`, "Authorization", authz)
	require.Equal(t, 200, rec.Code, rec.Body.String())
	m := jsonOf(t, rec)
	require.Nil(t, m["nickname"])
	require.Nil(t, m["learning_goal_note"])
	require.Equal(t, "TRAVEL", m["learning_goal"], "a key that is absent is left alone")
}

func TestPatchProfile_UnknownFieldRejected(t *testing.T) {
	e := newAPI(t)
	_, authz := e.profileUser(t, "hana@example.com")
	for name, body := range map[string]string{
		"top level":         `{"full_name":"Hana"}`,
		"nested":            `{"preferences":{"volume":3}}`,
		"flat preference":   `{"speech_speed":"SLOWER"}`,
		"known and unknown": `{"nickname":"Hana","bogus":1}`,
	} {
		rec := e.do(http.MethodPatch, profilePath, body, "Authorization", authz)
		require.Equal(t, 400, rec.Code, name+": "+rec.Body.String())
		require.Equal(t, "VALIDATION_FAILED", codeOf(t, rec), name)
	}
	rec := e.do(http.MethodGet, profilePath, "", "Authorization", authz)
	require.Nil(t, jsonOf(t, rec)["nickname"], "a rejected body changes nothing")
}

func TestPatchProfile_Validation(t *testing.T) {
	e := newAPI(t)
	_, authz := e.profileUser(t, "hana@example.com")
	long := strings.Repeat("あ", 31)
	for name, tc := range map[string]struct{ body, field, reason string }{
		"empty nickname":      {`{"nickname":"  "}`, "nickname", "out_of_range"},
		"nickname too long":   {`{"nickname":"` + long + `"}`, "nickname", "out_of_range"},
		"nickname braces":     {`{"nickname":"a{b"}`, "nickname", "invalid_format"},
		"bad level":           {`{"self_declared_level":"N9"}`, "self_declared_level", "invalid_format"},
		"bad enum in prefs":   {`{"preferences":{"formality":"RUDE"}}`, "preferences.formality", "invalid_format"},
		"note too long":       {`{"learning_goal_note":"` + strings.Repeat("x", 301) + `"}`, "learning_goal_note", "out_of_range"},
		"wrong type":          {`{"nickname":5}`, "nickname", "invalid_type"},
		"wrong type in prefs": {`{"preferences":{"memory_enabled":"yes"}}`, "preferences.memory_enabled", "invalid_type"},
		"null preferences":    {`{"preferences":null}`, "preferences", "invalid_type"},
	} {
		rec := e.do(http.MethodPatch, profilePath, tc.body, "Authorization", authz)
		require.Equal(t, 400, rec.Code, name+": "+rec.Body.String())
		require.Equal(t, "VALIDATION_FAILED", codeOf(t, rec), name)
		if tc.field != "" {
			require.Contains(t, rec.Body.String(), `"field":"`+tc.field+`","reason":"`+tc.reason+`"`, name)
		} else {
			require.Contains(t, rec.Body.String(), `"reason":"`+tc.reason+`"`, name)
		}
	}
}

func TestPatchProfile_ContentTypeRequired(t *testing.T) {
	e := newAPI(t)
	_, authz := e.profileUser(t, "hana@example.com")
	req := httptest.NewRequest(http.MethodPatch, profilePath, strings.NewReader(`{}`))
	req.Header.Set("Authorization", authz)
	rec := httptest.NewRecorder()
	e.handler.ServeHTTP(rec, req)
	require.Equal(t, 415, rec.Code)
}

func TestWriteEndpoints_Suspended403(t *testing.T) {
	e := newAPI(t)
	uid, authz := e.profileUser(t, "hana@example.com")
	mem := e.seedMemory(uid, "PROFILE_FACT", "job")
	e.suspend(uid)

	for _, c := range []struct{ method, path, body string }{
		{http.MethodPatch, profilePath, `{"nickname":"Hana"}`},
		{http.MethodDelete, profilePath + "/skills", ""},
		{http.MethodDelete, profilePath + "/memories", ""},
		{http.MethodDelete, profilePath + "/memories/" + mem.String(), ""},
	} {
		rec := e.do(c.method, c.path, c.body, "Authorization", authz)
		require.Equal(t, 403, rec.Code, c.method+" "+c.path+": "+rec.Body.String())
		require.Equal(t, "ACCOUNT_SUSPENDED", codeOf(t, rec))
	}
	_, still := e.memStore.Get(uid, mem)
	require.True(t, still, "a suspended user deletes nothing")
	require.Zero(t, e.memVer.Version(uid))

	// the read side stays open
	for _, p := range []string{"", "/skills", "/skills/GRAMMAR/history", "/memories"} {
		require.Equal(t, 200, e.do(http.MethodGet, profilePath+p, "", "Authorization", authz).Code, p)
	}
}

func TestProfileEndpoints_RateLimited(t *testing.T) {
	e := newAPI(t)
	_, authz := e.profileUser(t, "hana@example.com")
	e.limiter.deny["api_user_default"] = true
	id := uuid.NewString()
	for _, c := range []struct{ method, path, body string }{
		{http.MethodGet, profilePath, ""}, {http.MethodPatch, profilePath, `{}`}, {http.MethodGet, profilePath + "/skills", ""},
		{http.MethodGet, profilePath + "/skills/GRAMMAR/history", ""}, {http.MethodDelete, profilePath + "/skills", ""},
		{http.MethodGet, profilePath + "/memories", ""}, {http.MethodDelete, profilePath + "/memories", ""},
		{http.MethodDelete, profilePath + "/memories/" + id, ""},
	} {
		rec := e.do(c.method, c.path, c.body, "Authorization", authz)
		require.Equal(t, 429, rec.Code, c.method+" "+c.path)
		require.Equal(t, "RATE_LIMITED", codeOf(t, rec))
	}
}

func TestProfileEndpoints_AuthRequired(t *testing.T) {
	e := newAPI(t)
	id := uuid.NewString()
	for _, c := range []struct{ method, path string }{
		{http.MethodGet, profilePath}, {http.MethodPatch, profilePath}, {http.MethodGet, profilePath + "/skills"},
		{http.MethodGet, profilePath + "/skills/GRAMMAR/history"}, {http.MethodDelete, profilePath + "/skills"},
		{http.MethodGet, profilePath + "/memories"}, {http.MethodDelete, profilePath + "/memories"},
		{http.MethodDelete, profilePath + "/memories/" + id},
	} {
		body := ""
		if c.method == http.MethodPatch {
			body = `{}`
		}
		rec := e.do(c.method, c.path, body)
		require.Equal(t, 401, rec.Code, c.method+" "+c.path)
		require.Equal(t, "AUTH_REQUIRED", codeOf(t, rec))
	}
}

var (
	skillSession = uuid.MustParse("0192f1aa-1111-7abc-8def-000000000001")
	skillEvalIDs = []uuid.UUID{
		uuid.MustParse("0192f1b5-0000-7abc-8def-000000000001"),
		uuid.MustParse("0192f1b5-0000-7abc-8def-000000000002"),
		uuid.MustParse("0192f1b5-0000-7abc-8def-000000000003"),
	}
	skillAssessed = time.Date(2026, 10, 8, 11, 30, 0, 0, time.UTC)
)

// seedSkillEvidence gives GRAMMAR three estimates (oldest first) and the state they lead to.
func (e *apiEnv) seedSkillEvidence(t *testing.T, uid uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	d3 := 3
	ceil := 2.35
	require.NoError(t, e.learnerStore.WithinTx(ctx, func(ctx context.Context, tx learner.Tx) error {
		if err := tx.Profiles().Ensure(ctx, uid); err != nil {
			return err
		}
		levels := []struct{ l, c float64 }{{2.16, 0.280}, {2.27, 0.389}, {2.35, 0.502}}
		trends := []learner.Trend{learner.TrendInsufficient, learner.TrendInsufficient, learner.TrendImproving}
		for i, eid := range skillEvalIDs {
			at := skillAssessed.Add(time.Duration(i-2) * 24 * time.Hour)
			if _, err := tx.Estimates().Insert(ctx, uid, learner.EstimateRow{
				ID: uuid.MustParse("0192f1c5-0000-7abc-8def-00000000000" + string(rune('1'+i))), EvaluationID: eid, SessionID: skillSession, Skill: learner.SkillGrammar,
				AlgorithmVersion: 1, RawScore: 58, DEff: &d3, LObs: 2.66, CObs: 0.5, AssessedAt: at,
				Previous: levels[max(i-1, 0)].l, Level: levels[i].l, Confidence: levels[i].c, Count: i + 1, Trend: trends[i],
			}); err != nil {
				return err
			}
		}
		return tx.SkillStates().Save(ctx, uid, learner.StateRow{
			Skill: learner.SkillGrammar, PriorLevel: 2.0, PriorConfidence: 0.1, Level: 2.35, Confidence: 0.502, Ceiling: &ceil,
			Trend: learner.TrendImproving, EvidenceCount: 3, LastEvaluationID: &skillEvalIDs[2], LastEvidenceAt: &skillAssessed,
		}, skillAssessed)
	}))
}

func TestSkillsGolden(t *testing.T) {
	e := newAPI(t)
	uid, authz := e.profileUser(t, "hana@example.com")
	require.Equal(t, 200, e.do(http.MethodPatch, profilePath, `{"self_declared_level":"N4"}`, "Authorization", authz).Code)
	e.seedSkillEvidence(t, uid)
	e.learnerStore.SetExposure(uid, []learner.ExposureRow{{Gender: "FEMALE", Accent: "KANTO", Sessions: 5, Seconds: 3000}, {Gender: "MALE", Accent: "KANSAI", Sessions: 2, Seconds: 1840}})

	rec := e.do(http.MethodGet, profilePath+"/skills", "", "Authorization", authz)
	require.Equal(t, 200, rec.Code, rec.Body.String())
	goldenFile(t, "profile", "skills_200.json", rec.Body.Bytes())

	m := jsonOf(t, rec)
	require.NotContains(t, m, "data", "composite object, not an envelope")
	skills := m["skills"].([]any)
	require.Len(t, skills, 6)
	for i, want := range []string{"GRAMMAR", "VOCABULARY", "FLUENCY", "NATURALNESS", "LISTENING_RESPONSE", "INTERACTION_COMPLEXITY"} {
		require.Equal(t, want, skills[i].(map[string]any)["skill"])
	}
	g := skills[0].(map[string]any)
	require.Equal(t, 2.35, g["estimated_level"])
	require.Equal(t, float64(3), g["evidence_count"])
	require.Equal(t, "IMPROVING", g["trend"])
	require.Equal(t, "2026-10-08T11:30:00.000Z", g["last_assessed_at"])
	v := skills[1].(map[string]any)
	require.Equal(t, true, v["is_provisional"])
	require.Equal(t, "INSUFFICIENT_DATA", v["trend"])
	require.Nil(t, v["last_assessed_at"])
	require.Equal(t, float64(1), m["algorithm_version"])
}

func TestSkills_NewUserPriors(t *testing.T) {
	e := newAPI(t)
	_, authz := e.profileUser(t, "hana@example.com")
	rec := e.do(http.MethodGet, profilePath+"/skills", "", "Authorization", authz)
	require.Equal(t, 200, rec.Code, rec.Body.String())
	m := jsonOf(t, rec)
	require.Equal(t, []any{}, m["focus_skills"])
	require.Equal(t, []any{}, m["voice_exposure"].(map[string]any)["items"])
	require.Nil(t, m["voice_exposure"].(map[string]any)["exposure_suggestion"])
	for _, s := range m["skills"].([]any) {
		sk := s.(map[string]any)
		require.Equal(t, true, sk["is_provisional"])
		require.Equal(t, float64(0), sk["evidence_count"])
	}
}

func TestHistoryGolden(t *testing.T) {
	e := newAPI(t)
	uid, authz := e.profileUser(t, "hana@example.com")
	e.seedSkillEvidence(t, uid)

	rec := e.do(http.MethodGet, profilePath+"/skills/GRAMMAR/history", "", "Authorization", authz)
	require.Equal(t, 200, rec.Code, rec.Body.String())
	goldenFile(t, "profile", "history_200.json", rec.Body.Bytes())
	m := jsonOf(t, rec)
	require.Equal(t, "GRAMMAR", m["skill"])
	require.Nil(t, m["next_cursor"])
	data := m["data"].([]any)
	require.Len(t, data, 3)
	require.Equal(t, 2.35, data[0].(map[string]any)["estimated_level"], "newest first")
	require.Equal(t, 2.16, data[2].(map[string]any)["estimated_level"])

	// another skill has no history
	rec = e.do(http.MethodGet, profilePath+"/skills/FLUENCY/history", "", "Authorization", authz)
	require.Equal(t, 200, rec.Code)
	require.Equal(t, []any{}, jsonOf(t, rec)["data"])
}

func TestHistory_CursorPagination(t *testing.T) {
	e := newAPI(t)
	uid, authz := e.profileUser(t, "hana@example.com")
	e.seedSkillEvidence(t, uid)

	var got []float64
	url := profilePath + "/skills/GRAMMAR/history?limit=2"
	pages := 0
	for url != "" {
		rec := e.do(http.MethodGet, url, "", "Authorization", authz)
		require.Equal(t, 200, rec.Code, rec.Body.String())
		m := jsonOf(t, rec)
		pages++
		for _, it := range m["data"].([]any) {
			got = append(got, it.(map[string]any)["estimated_level"].(float64))
		}
		url = ""
		if c, ok := m["next_cursor"].(string); ok {
			require.Len(t, m["data"], 2)
			url = profilePath + "/skills/GRAMMAR/history?limit=2&cursor=" + c
		}
	}
	require.Equal(t, 2, pages)
	require.Equal(t, []float64{2.35, 2.27, 2.16}, got)
}

func TestHistory_InvalidSkill400(t *testing.T) {
	e := newAPI(t)
	_, authz := e.profileUser(t, "hana@example.com")
	for _, s := range []string{"grammar", "SPEAKING", "TASK_ACHIEVEMENT", "GRAMMAR%20"} {
		rec := e.do(http.MethodGet, profilePath+"/skills/"+s+"/history", "", "Authorization", authz)
		require.Equal(t, 400, rec.Code, s+": "+rec.Body.String())
		require.Equal(t, "VALIDATION_FAILED", codeOf(t, rec), s)
	}
}

func TestHistory_LimitAndCursorValidation(t *testing.T) {
	e := newAPI(t)
	_, authz := e.profileUser(t, "hana@example.com")
	for _, q := range []string{"limit=0", "limit=101", "limit=-1", "limit=abc"} {
		rec := e.do(http.MethodGet, profilePath+"/skills/GRAMMAR/history?"+q, "", "Authorization", authz)
		require.Equal(t, 400, rec.Code, q)
		require.Equal(t, "VALIDATION_FAILED", codeOf(t, rec), q)
	}
	require.Equal(t, 200, e.do(http.MethodGet, profilePath+"/skills/GRAMMAR/history?limit=100", "", "Authorization", authz).Code)

	for name, c := range map[string]string{
		"not base64":    "!!!",
		"not json":      encode(`nope`),
		"wrong shape":   encode(`{"v":1,"k":"x"}`),
		"one element":   encode(`{"v":1,"k":["2026-10-08T11:30:00Z"]}`),
		"bad time":      encode(`{"v":1,"k":["yesterday","` + uuid.NewString() + `"]}`),
		"bad id":        encode(`{"v":1,"k":["2026-10-08T11:30:00Z","x"]}`),
		"nil id":        encode(`{"v":1,"k":["2026-10-08T11:30:00Z","` + uuid.Nil.String() + `"]}`),
		"wrong version": encode(`{"v":2,"k":["2026-10-08T11:30:00Z","` + uuid.NewString() + `"]}`),
	} {
		rec := e.do(http.MethodGet, profilePath+"/skills/GRAMMAR/history?cursor="+c, "", "Authorization", authz)
		require.Equal(t, 400, rec.Code, name+": "+rec.Body.String())
		require.Equal(t, "INVALID_CURSOR", codeOf(t, rec), name)
	}
}

func TestDeleteSkills_Idempotent204(t *testing.T) {
	e := newAPI(t)
	uid, authz := e.profileUser(t, "hana@example.com")
	e.seedSkillEvidence(t, uid)
	for i := 0; i < 2; i++ {
		rec := e.do(http.MethodDelete, profilePath+"/skills", "", "Authorization", authz)
		require.Equal(t, 204, rec.Code, rec.Body.String())
		require.Empty(t, rec.Body.String())
	}
	require.Empty(t, e.learnerStore.Estimates(uid))
	for _, s := range e.learnerStore.States(uid) {
		require.Zero(t, s.EvidenceCount)
		require.Equal(t, learner.TrendInsufficient, s.Trend)
		require.Equal(t, s.PriorLevel, s.Level)
	}
}

func TestProfileEndpoints_OnlyOwnData(t *testing.T) {
	e := newAPI(t)
	a, authA := e.profileUser(t, "a@example.com")
	b, authB := e.profileUser(t, "b@example.com")
	e.seedSkillEvidence(t, b)
	require.Equal(t, 200, e.do(http.MethodPatch, profilePath, `{"nickname":"Bima"}`, "Authorization", authB).Code)

	rec := e.do(http.MethodGet, profilePath+"/skills/GRAMMAR/history", "", "Authorization", authA)
	require.Equal(t, []any{}, jsonOf(t, rec)["data"], "A sees none of the estimates of B")
	rec = e.do(http.MethodGet, profilePath, "", "Authorization", authA)
	m := jsonOf(t, rec)
	require.Equal(t, a.String(), m["user_id"])
	require.Nil(t, m["nickname"])
	for _, s := range jsonOf(t, e.do(http.MethodGet, profilePath+"/skills", "", "Authorization", authA))["skills"].([]any) {
		require.Zero(t, s.(map[string]any)["evidence_count"])
	}
	require.Equal(t, 204, e.do(http.MethodDelete, profilePath+"/skills", "", "Authorization", authA).Code)
	require.Len(t, e.learnerStore.Estimates(b), 3, "the reset of A leaves B alone")
}

// encode wraps raw JSON as an opaque cursor.
func encode(raw string) string { return base64.RawURLEncoding.EncodeToString([]byte(raw)) }
