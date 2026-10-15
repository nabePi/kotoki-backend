package httpapi_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/content"
)

var (
	cTopicID     = uuid.MustParse("01920000-0001-7000-8000-000000000001")
	cScenarioID  = uuid.MustParse("01920000-0002-7000-8000-000000000003")
	cVersionID   = uuid.MustParse("01920000-0003-7000-8000-000000000003")
	cCharacterID = uuid.MustParse("01920000-000a-7000-8000-000000000001")
	cPromptID    = uuid.MustParse("01920000-000d-7000-8000-000000000001")
)

// seedContent puts one usable topic, scenario, character and a CONVERSATION prompt in the fake
// repository, with the ids of the seed (spec 6.13.5).
func (e *apiEnv) seedContent() {
	e.content.PutTopic(content.Topic{ID: cTopicID, Code: "JOB_INTERVIEW", Name: "Job Interview", NameJA: "就職面接",
		Description: "Practice interviews at Japanese companies.", IconKey: strPtr("briefcase"), SortOrder: 10, Status: content.StatusActive})
	e.content.PutScenario(content.Scenario{ID: cScenarioID, TopicID: cTopicID, Code: "BACKEND_ENGINEER_INTERVIEW", Name: "Backend Engineer Interview",
		NameJA: "バックエンドエンジニアの面接", Description: "A technical interview for a backend engineer role.", SortOrder: 30, Status: content.StatusActive})
	e.content.PutScenarioVersion(content.ScenarioVersion{
		ID: cVersionID, ScenarioID: cScenarioID, Version: 1, Status: content.VersionPublished, DifficultyLevel: 4,
		RecommendedLevelMin: 3.0, RecommendedLevelMax: 4.5,
		Situation: "A second-round technical interview.", AIRole: "Engineering manager", UserRole: "Candidate", Goal: "Explain a technical decision.",
		Tasks: []string{"Introduce yourself"}, Constraints: []string{"never shown to the client"}, GrammarTargets: []string{"〜ために"},
		VocabHints:      []content.VocabHint{{TermJA: "経験", Reading: "けいけん", Meaning: "experience"}},
		EvaluationFocus: []string{"VOCABULARY"}, Formality: content.FormalityKeigo, OpeningLine: "本日はお越しいただきありがとうございます。",
		VisualPrompt: content.VisualPrompt{Headline: "You enter the interview room", Body: "Greet the interviewer.", StarterPhrasesJA: []string{"本日はよろしくお願いいたします。"}, ImageKey: strPtr("interview_room")},
	})
	e.content.PutCharacter(content.Character{ID: cCharacterID, Code: "HARUKA", Status: content.StatusActive, SortOrder: 10, Name: "Haruka", AvatarKey: strPtr("haruka"),
		Persona: strings.Repeat("A warm and curious guide who loves cafés. ", 2), ProviderName: "ELEVENLABS", ModelName: "Eleven v3 Conversational", VoiceID: "21m00Tcm4TlvDq8ikWAM",
		Voice: content.Voice{Gender: content.GenderFemale, Accent: content.AccentKanto, SpeakingRate: 1.0}})
	e.content.PutPromptVersion(content.PromptVersion{ID: cPromptID, PromptConfigID: uuid.MustParse("01920000-000c-7000-8000-000000000001"), Key: content.PromptConversation,
		Version: 1, Status: content.VersionPublished, SystemTemplate: "x"})
}

func strPtr(s string) *string { return &s }

func (e *apiEnv) userAuthz(t *testing.T) string {
	t.Helper()
	return bearerOf(t, e.register(t, "hana@example.com"))
}

func TestContentCatalogGolden(t *testing.T) {
	e := newAPI(t)
	e.seedContent()
	authz := e.userAuthz(t)
	for _, c := range []struct{ name, path string }{
		{"topics_200.json", "/api/v1/topics"},
		{"scenarios_200.json", "/api/v1/scenarios?topic_id=" + cTopicID.String()},
		{"scenario_200.json", "/api/v1/scenarios/" + cScenarioID.String()},
		{"characters_200.json", "/api/v1/characters"},
		{"character_200.json", "/api/v1/characters/" + cCharacterID.String()},
	} {
		rec := e.do(http.MethodGet, c.path, "", "Authorization", authz)
		require.Equal(t, 200, rec.Code, c.path+" "+rec.Body.String())
		require.Equal(t, "private, max-age=60", rec.Header().Get("Cache-Control"), c.path)
		goldenFile(t, "content", c.name, rec.Body.Bytes())
	}
}

func TestContentListEnvelopeIsItems(t *testing.T) {
	e := newAPI(t)
	authz := e.userAuthz(t)
	for _, p := range []string{"/topics", "/scenarios", "/characters"} {
		rec := e.do(http.MethodGet, "/api/v1"+p, "", "Authorization", authz)
		require.Equal(t, 200, rec.Code)
		require.JSONEq(t, `{"items":[]}`, rec.Body.String(), p)
	}
}

func TestCharacterResponseHasNoVoiceIDs(t *testing.T) {
	e := newAPI(t)
	e.seedContent()
	authz := e.userAuthz(t)
	for _, p := range []string{"/characters", "/characters/" + cCharacterID.String(), "/scenarios"} {
		body := e.do(http.MethodGet, "/api/v1"+p, "", "Authorization", authz).Body.String()
		for _, key := range []string{"voice", "persona", "provider_name", "model_name", "voice_id", "speaking_rate", "greeting_hint", "fallback", "opening_line", "constraints"} {
			require.NotContains(t, body, `"`+key+`"`, p)
		}
		require.NotContains(t, body, "21m00Tcm4TlvDq8ikWAM")
	}
	var one map[string]any
	rec := e.do(http.MethodGet, "/api/v1/characters/"+cCharacterID.String(), "", "Authorization", authz)
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &one))
	require.ElementsMatch(t, []string{"id", "code", "name", "avatar_key", "gender", "accent"}, keysOf(one))
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestContentCatalogErrors(t *testing.T) {
	e := newAPI(t)
	e.seedContent()
	authz := e.userAuthz(t)
	get := func(p string) (int, string) {
		rec := e.do(http.MethodGet, "/api/v1"+p, "", "Authorization", authz)
		return rec.Code, codeOf(t, rec)
	}
	st, code := get("/scenarios?topic_id=bukan-uuid")
	require.Equal(t, 400, st)
	require.Equal(t, "VALIDATION_FAILED", code)
	st, code = get("/scenarios/" + uuid.NewString())
	require.Equal(t, 404, st)
	require.Equal(t, "NOT_FOUND", code)
	st, code = get("/characters/" + uuid.NewString())
	require.Equal(t, 404, st)
	require.Equal(t, "NOT_FOUND", code)
	st, _ = get("/scenarios/not-a-uuid")
	require.Equal(t, 404, st)
	for _, p := range []string{"/conversation-styles", "/expression-profiles"} {
		st, code = get(p)
		require.Equal(t, 404, st, p)
		require.Equal(t, "NOT_FOUND", code, p)
	}
	rec := e.do(http.MethodGet, "/api/v1/topics", "")
	require.Equal(t, "AUTH_REQUIRED", codeOf(t, rec))
}

func TestAdminContentRequiresAdmin(t *testing.T) {
	e := newAPI(t)
	e.seedContent()
	authz := e.userAuthz(t)
	rec := e.do(http.MethodGet, "/api/v1/admin/content/characters/"+cCharacterID.String(), "", "Authorization", authz)
	require.Equal(t, 403, rec.Code)
	require.Equal(t, "FORBIDDEN", codeOf(t, rec))
}

func TestAdminCharacterGetAndPatch(t *testing.T) {
	e := newAPI(t)
	e.seedContent()
	admin, _ := e.adminAuthz(t)
	path := "/api/v1/admin/content/characters/" + cCharacterID.String()

	rec := e.do(http.MethodGet, path, "", "Authorization", admin)
	require.Equal(t, 200, rec.Code, rec.Body.String())
	var got map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	for _, k := range []string{"persona", "provider_name", "model_name", "voice_id", "voice"} {
		require.Contains(t, got, k)
	}
	goldenFile(t, "content", "admin_character_200.json", rec.Body.Bytes())

	rec = e.do(http.MethodPatch, path, `{"provider_name":"cartesia","model_name":"sonic3.6","voice_id":"db6b0ed5"}`, "Authorization", admin)
	require.Equal(t, 200, rec.Code, rec.Body.String())
	var upd struct {
		Character struct {
			ProviderName string `json:"provider_name"`
			Persona      string `json:"persona"`
		} `json:"character"`
		Warnings []any `json:"warnings"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &upd))
	require.Equal(t, "CARTESIA", upd.Character.ProviderName)
	require.NotEmpty(t, upd.Character.Persona)
	require.NotNil(t, upd.Warnings, "warnings is always an array")

	rec = e.do(http.MethodPatch, path, `{"voice":{"gender":"MALE","accent":"KANSAI","speaking_rate":0.9}}`, "Authorization", admin)
	require.Equal(t, 200, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), `"accent":"KANSAI"`)
	require.NotContains(t, rec.Body.String(), `"fallback"`)

	rec = e.do(http.MethodPatch, path, `{"avatar_key":null}`, "Authorization", admin)
	require.Equal(t, 200, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), `"avatar_key":null`)
}

func TestAdminCharacterPatchErrors(t *testing.T) {
	e := newAPI(t)
	e.seedContent()
	admin, _ := e.adminAuthz(t)
	path := "/api/v1/admin/content/characters/" + cCharacterID.String()

	for _, body := range []string{`{"code":"NEW"}`, `{"id":"` + uuid.NewString() + `"}`} {
		rec := e.do(http.MethodPatch, path, body, "Authorization", admin)
		require.Equal(t, 400, rec.Code, body)
		require.Equal(t, "VALIDATION_FAILED", codeOf(t, rec))
	}

	rec := e.do(http.MethodPatch, path, `{"voice":{"gender":"FEMALE","accent":"KANTO","speaking_rate":1.5}}`, "Authorization", admin)
	require.Equal(t, 422, rec.Code, rec.Body.String())
	var out struct {
		Error struct {
			Code    string `json:"code"`
			Details []struct {
				Field string `json:"field"`
				Rule  string `json:"rule"`
			} `json:"details"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.Equal(t, "CONTENT_VALIDATION_FAILED", out.Error.Code)
	require.Equal(t, "voice.speaking_rate", out.Error.Details[0].Field)

	rec = e.do(http.MethodPatch, path, `{"voice":{"gender":"FEMALE","accent":"KANTO","speaking_rate":1,"providers":{}}}`, "Authorization", admin)
	require.Equal(t, 422, rec.Code, "an unknown voice key is a content error")

	rec = e.do(http.MethodPatch, "/api/v1/admin/content/characters/"+uuid.NewString(), `{"name":"x"}`, "Authorization", admin)
	require.Equal(t, 404, rec.Code)

	rec = e.do(http.MethodPatch, path+"/status", `{"status":"DISABLED"}`, "Authorization", admin)
	require.Equal(t, 404, rec.Code, "characters have no status endpoint")
	require.Equal(t, "NOT_FOUND", codeOf(t, rec))
}

func TestAdminVersionLifecycleOverHTTP(t *testing.T) {
	e := newAPI(t)
	e.seedContent()
	admin, _ := e.adminAuthz(t)
	base := "/api/v1/admin/content/scenarios/" + cScenarioID.String() + "/versions"

	rec := e.do(http.MethodPost, base, `{"copy_from_version_id":"`+cVersionID.String()+`","formality":"POLITE","notes":"Softer register"}`, "Authorization", admin)
	require.Equal(t, 201, rec.Code, rec.Body.String())
	var created struct {
		ID        uuid.UUID `json:"id"`
		Version   int       `json:"version"`
		Status    string    `json:"status"`
		Formality string    `json:"formality"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))
	require.Equal(t, 2, created.Version)
	require.Equal(t, "DRAFT", created.Status)
	require.Equal(t, "POLITE", created.Formality)

	rec = e.do(http.MethodGet, base, "", "Authorization", admin)
	require.Equal(t, 200, rec.Code)
	require.Contains(t, rec.Body.String(), `"items"`)

	rec = e.do(http.MethodGet, base+"/"+created.ID.String(), "", "Authorization", admin)
	require.Equal(t, 200, rec.Code)

	rec = e.do(http.MethodPost, base+"/"+created.ID.String()+"/publish", "", "Authorization", admin)
	require.Equal(t, 200, rec.Code, rec.Body.String())
	var pub struct {
		Version          struct{ Status string } `json:"version"`
		RetiredVersionID *uuid.UUID              `json:"retired_version_id"`
		Warnings         []any                   `json:"warnings"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &pub))
	require.Equal(t, "PUBLISHED", pub.Version.Status)
	require.Equal(t, cVersionID, *pub.RetiredVersionID)

	rec = e.do(http.MethodPost, base+"/"+cVersionID.String()+"/retire", "", "Authorization", admin)
	require.Equal(t, 409, rec.Code, "the old version is already RETIRED")
	require.Equal(t, "CONTENT_INVALID_TRANSITION", codeOf(t, rec))

	rec = e.do(http.MethodDelete, base+"/"+created.ID.String(), "", "Authorization", admin)
	require.Equal(t, 409, rec.Code, "a PUBLISHED version cannot be deleted")

	rec = e.do(http.MethodPost, base, `{"copy_from_version_id":"`+cVersionID.String()+`"}`, "Authorization", admin)
	require.Equal(t, 201, rec.Code)
	var draft struct{ ID uuid.UUID }
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &draft))
	rec = e.do(http.MethodDelete, base+"/"+draft.ID.String(), "", "Authorization", admin)
	require.Equal(t, 204, rec.Code)
	require.Empty(t, rec.Body.String())
}

func TestAdminPublishValidationFailureHasIssueArray(t *testing.T) {
	e := newAPI(t)
	e.seedContent()
	admin, _ := e.adminAuthz(t)
	bad := content.ScenarioVersion{ID: uuid.New(), ScenarioID: cScenarioID, Version: 2, Status: content.VersionDraft, DifficultyLevel: 2,
		RecommendedLevelMin: 1, RecommendedLevelMax: 2.5, Situation: "", AIRole: "a", UserRole: "u", Goal: "g", Formality: content.FormalityPolite, OpeningLine: "こんにちは。"}
	e.content.PutScenarioVersion(bad)
	rec := e.do(http.MethodPost, "/api/v1/admin/content/scenarios/"+cScenarioID.String()+"/versions/"+bad.ID.String()+"/publish", "", "Authorization", admin)
	require.Equal(t, 422, rec.Code, rec.Body.String())
	var out struct {
		Error struct {
			Code    string            `json:"code"`
			Details []content.Issue   `json:"details"`
			Extra   map[string]string `json:"-"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.Equal(t, "CONTENT_VALIDATION_FAILED", out.Error.Code)
	require.NotEmpty(t, out.Error.Details)
	require.Equal(t, "situation", out.Error.Details[0].Field)
	require.Equal(t, content.SeverityError, out.Error.Details[0].Severity)
}

func TestAdminCreateDraftRejectsUnknownField(t *testing.T) {
	e := newAPI(t)
	e.seedContent()
	admin, _ := e.adminAuthz(t)
	rec := e.do(http.MethodPost, "/api/v1/admin/content/prompts/CONVERSATION/versions", `{"template":"old field"}`, "Authorization", admin)
	require.Equal(t, 400, rec.Code, rec.Body.String())
	require.Equal(t, "VALIDATION_FAILED", codeOf(t, rec))
}

func TestAdminSetStatusAndValidationReport(t *testing.T) {
	e := newAPI(t)
	e.seedContent()
	admin, _ := e.adminAuthz(t)
	rec := e.do(http.MethodPatch, "/api/v1/admin/content/topics/"+cTopicID.String()+"/status", `{"status":"DISABLED"}`, "Authorization", admin)
	require.Equal(t, 200, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), `"status":"DISABLED"`)
	rec = e.do(http.MethodPatch, "/api/v1/admin/content/topics/"+cTopicID.String()+"/status", `{"status":"WEIRD"}`, "Authorization", admin)
	require.Equal(t, 400, rec.Code)
	rec = e.do(http.MethodGet, "/api/v1/admin/content/validation-report", "", "Authorization", admin)
	require.Equal(t, 200, rec.Code, rec.Body.String())
	var rep struct {
		Errors   []any `json:"errors"`
		Warnings []any `json:"warnings"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rep))
	require.NotNil(t, rep.Errors)
	require.NotNil(t, rep.Warnings)
}
