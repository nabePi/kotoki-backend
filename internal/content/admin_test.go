package content_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/content"
	"github.com/nabePi/kotoki-backend/internal/platform/clock"
	"github.com/nabePi/kotoki-backend/internal/platform/errs"
	"github.com/nabePi/kotoki-backend/internal/platform/idgen"
	"github.com/nabePi/kotoki-backend/internal/testutil/fakes"
)

type invCounter struct{ n int }

func (i *invCounter) Invalidate() { i.n++ }

type adminEnv struct {
	w     *world
	store *fakes.ContentStore
	svc   *content.AdminService
	clk   *clock.Fake
	inv   *invCounter
	actor uuid.UUID
}

func newAdminEnv(t *testing.T, mod ...func(*content.AdminDeps)) *adminEnv {
	t.Helper()
	registerFixtures(t)
	w := newWorld()
	w.prompt.PromptConfigID = uuid.New()
	w.put()
	e := &adminEnv{w: w, store: fakes.NewContentStore(w.repo), clk: clock.NewFake(t0), inv: &invCounter{}, actor: uuid.New()}
	d := content.AdminDeps{Store: e.store, Clock: e.clk, IDs: idgen.NewV7(), Invalidator: e.inv,
		Validator: content.ValidatorConfig{TTSEnabled: func(string) bool { return true }},
		RequestID: func(context.Context) string { return "req-1" }}
	for _, m := range mod {
		m(&d)
	}
	e.svc = content.NewAdminService(d)
	return e
}

func (e *adminEnv) draftScenario(t *testing.T, version int, mod ...func(*content.ScenarioVersion)) content.ScenarioVersion {
	t.Helper()
	sv := validScenario()
	sv.ScenarioID, sv.Version, sv.Status = e.w.scenario.ID, version, content.VersionDraft
	for _, m := range mod {
		m(&sv)
	}
	e.w.repo.PutScenarioVersion(sv)
	return sv
}

func (e *adminEnv) status(id uuid.UUID) content.VersionStatus {
	sv, err := e.w.repo.GetScenarioVersion(context.Background(), id)
	if err != nil {
		panic(err)
	}
	return sv.Status
}

func (e *adminEnv) scenarioParent() string { return e.w.scenario.ID.String() }

func code(err error) errs.Code { return errs.CodeOf(err) }

func actions(s *fakes.ContentStore) []string {
	var out []string
	for _, a := range s.Audit() {
		out = append(out, a.Action)
	}
	return out
}

func TestPublishRetiresPreviousInSameTx(t *testing.T) {
	e := newAdminEnv(t)
	d := e.draftScenario(t, 2)
	res, err := e.svc.Publish(context.Background(), e.actor, content.FamilyScenarios, e.scenarioParent(), d.ID)
	require.NoError(t, err)
	require.Equal(t, content.VersionPublished, e.status(d.ID))
	require.Equal(t, content.VersionRetired, e.status(e.w.sv.ID))
	require.NotNil(t, res.RetiredVersionID)
	require.Equal(t, e.w.sv.ID, *res.RetiredVersionID)
	require.Equal(t, content.VersionPublished, res.Version.Status)
	require.Equal(t, []string{content.ActionVersionPublished}, actions(e.store))
	a := e.store.Audit()[0]
	require.Equal(t, content.TargetScenario, a.TargetType)
	require.Equal(t, d.ID.String(), a.TargetID)
	require.Equal(t, e.actor, a.AdminUserID)
	require.Equal(t, "req-1", a.RequestID)
	require.Equal(t, 1, e.inv.n, "the cache is dropped after commit")
}

func TestPublishIdempotent(t *testing.T) {
	e := newAdminEnv(t)
	res, err := e.svc.Publish(context.Background(), e.actor, content.FamilyScenarios, e.scenarioParent(), e.w.sv.ID)
	require.NoError(t, err)
	require.Nil(t, res.RetiredVersionID)
	require.Empty(t, e.store.Audit(), "no new audit entry")
	require.Zero(t, e.inv.n)
}

func TestPublishValidationError422WithIssueArray(t *testing.T) {
	e := newAdminEnv(t)
	d := e.draftScenario(t, 2, func(sv *content.ScenarioVersion) { sv.Situation = "" })
	_, err := e.svc.Publish(context.Background(), e.actor, content.FamilyScenarios, e.scenarioParent(), d.ID)
	require.Equal(t, errs.CodeContentValidationFailed, code(err))
	issues, ok := errs.DetailsOf(err).([]content.Issue)
	require.True(t, ok)
	require.NotEmpty(t, issues)
	require.Equal(t, "situation", issues[0].Field)
	require.Equal(t, content.VersionDraft, e.status(d.ID))
	require.Equal(t, content.VersionPublished, e.status(e.w.sv.ID), "nothing changed")
	require.Empty(t, e.store.Audit())
}

func TestPublishWrongParent404(t *testing.T) {
	e := newAdminEnv(t)
	other := content.Scenario{ID: uuid.New(), TopicID: e.w.topic.ID, Code: "OTHER", Status: content.StatusActive}
	e.w.repo.PutScenario(other)
	d := e.draftScenario(t, 2)
	_, err := e.svc.Publish(context.Background(), e.actor, content.FamilyScenarios, other.ID.String(), d.ID)
	require.Equal(t, errs.CodeNotFound, code(err))
	_, err = e.svc.Publish(context.Background(), e.actor, content.FamilyScenarios, uuid.NewString(), d.ID)
	require.Equal(t, errs.CodeNotFound, code(err))
	_, err = e.svc.Publish(context.Background(), e.actor, content.Family("characters"), e.scenarioParent(), d.ID)
	require.Equal(t, errs.CodeNotFound, code(err))
}

func TestRollbackRetiredToPublished(t *testing.T) {
	e := newAdminEnv(t)
	d := e.draftScenario(t, 2)
	_, err := e.svc.Publish(context.Background(), e.actor, content.FamilyScenarios, e.scenarioParent(), d.ID)
	require.NoError(t, err)
	res, err := e.svc.Publish(context.Background(), e.actor, content.FamilyScenarios, e.scenarioParent(), e.w.sv.ID)
	require.NoError(t, err)
	require.Equal(t, content.VersionPublished, e.status(e.w.sv.ID))
	require.Equal(t, content.VersionRetired, e.status(d.ID))
	require.Equal(t, d.ID, *res.RetiredVersionID)
	require.Nil(t, res.Version.RetiredAt, "retired_at is cleared on re-publish")
}

func TestPublishPromptDraft(t *testing.T) {
	e := newAdminEnv(t)
	pv := validPrompt(content.PromptConversation)
	pv.PromptConfigID, pv.Version = e.w.prompt.PromptConfigID, 2
	e.w.repo.PutPromptVersion(pv)
	res, err := e.svc.Publish(context.Background(), e.actor, content.FamilyPrompts, "conversation", pv.ID)
	require.NoError(t, err)
	require.Equal(t, e.w.prompt.ID, *res.RetiredVersionID)
	require.Equal(t, content.TargetPrompt, e.store.Audit()[0].TargetType)
}

func TestRetirePromptWithoutReplacement409(t *testing.T) {
	e := newAdminEnv(t)
	_, err := e.svc.Retire(context.Background(), e.actor, content.FamilyPrompts, "CONVERSATION", e.w.prompt.ID)
	require.Equal(t, errs.CodeContentVersionRequired, code(err))
	pv, _ := e.w.repo.GetPromptVersion(context.Background(), e.w.prompt.ID)
	require.Equal(t, content.VersionPublished, pv.Status)
	require.Empty(t, e.store.Audit())
}

func TestRetireScenarioWithoutReplacementAllowedWithWarnings(t *testing.T) {
	e := newAdminEnv(t)
	res, err := e.svc.Retire(context.Background(), e.actor, content.FamilyScenarios, e.scenarioParent(), e.w.sv.ID)
	require.NoError(t, err)
	require.Equal(t, content.VersionRetired, res.Version.Status)
	require.NotEmpty(t, res.Warnings)
	require.Equal(t, []string{content.ActionVersionRetired}, actions(e.store))
	require.Equal(t, 1, e.inv.n)

	_, err = e.svc.Retire(context.Background(), e.actor, content.FamilyScenarios, e.scenarioParent(), e.w.sv.ID)
	require.Equal(t, errs.CodeContentInvalidTransition, code(err), "only a PUBLISHED version can be retired")
}

func TestDeleteNonDraft409(t *testing.T) {
	e := newAdminEnv(t)
	err := e.svc.DeleteDraft(context.Background(), e.actor, content.FamilyScenarios, e.scenarioParent(), e.w.sv.ID)
	require.Equal(t, errs.CodeContentInvalidTransition, code(err))
	require.Empty(t, e.store.Audit())

	d := e.draftScenario(t, 2)
	require.NoError(t, e.svc.DeleteDraft(context.Background(), e.actor, content.FamilyScenarios, e.scenarioParent(), d.ID))
	_, err = e.w.repo.GetScenarioVersion(context.Background(), d.ID)
	require.ErrorIs(t, err, content.ErrNotFound)
	require.Equal(t, []string{content.ActionVersionDeleted}, actions(e.store))
}

func scenarioJSON(t *testing.T, over map[string]any) json.RawMessage {
	t.Helper()
	m := map[string]any{
		"difficulty_level": 2, "recommended_level_min": 1.0, "recommended_level_max": 2.5,
		"situation": "You order lunch at a small restaurant.", "ai_role": "A waiter", "user_role": "A customer", "goal": "Order a meal",
		"tasks": []string{"order food"}, "constraints": []string{"one question at a time"}, "grammar_targets": []string{"〜をください"},
		"vocab_hints": []map[string]string{{"term_ja": "注文", "reading": "ちゅうもん", "meaning": "order"}}, "evaluation_focus": []string{"GRAMMAR"},
		"formality": "POLITE", "opening_line": "いらっしゃいませ。",
		"visual_prompt": map[string]any{"headline": "Lunch", "body": "Order something", "starter_phrases_ja": []string{"すみません"}, "image_key": nil},
	}
	for k, v := range over {
		m[k] = v
	}
	b, err := json.Marshal(m)
	require.NoError(t, err)
	return b
}

func TestCreateDraftNumbersVersionsAndAudits(t *testing.T) {
	e := newAdminEnv(t)
	v, err := e.svc.CreateDraft(context.Background(), e.actor, content.FamilyScenarios, e.scenarioParent(), scenarioJSON(t, nil), nil)
	require.NoError(t, err)
	require.Equal(t, 2, v.Version)
	require.Equal(t, content.VersionDraft, v.Status)
	require.Equal(t, e.clk.Now(), v.CreatedAt)
	require.Equal(t, []string{content.ActionVersionCreated}, actions(e.store))
	require.Zero(t, e.inv.n, "a draft is not visible to learners: no cache drop")
}

func TestCreateDraftCopyFrom(t *testing.T) {
	e := newAdminEnv(t)
	body := json.RawMessage(`{"formality":"CASUAL","notes":"Softer register"}`)
	v, err := e.svc.CreateDraft(context.Background(), e.actor, content.FamilyScenarios, e.scenarioParent(), body, &e.w.sv.ID)
	require.NoError(t, err)
	require.Equal(t, content.FormalityCasual, v.Scenario.Formality, "overridden")
	require.Equal(t, e.w.sv.Situation, v.Scenario.Situation, "copied")
	require.Equal(t, e.w.sv.OpeningLine, v.Scenario.OpeningLine)
	require.NotNil(t, v.Scenario.Notes)

	body = json.RawMessage(`{"copy_from_version_id":"` + e.w.sv.ID.String() + `"}`)
	v2, err := e.svc.CreateDraft(context.Background(), e.actor, content.FamilyScenarios, e.scenarioParent(), body, nil)
	require.NoError(t, err)
	require.Equal(t, content.FormalityKeigo, v2.Scenario.Formality, "formality is copied too")
	require.Equal(t, 3, v2.Version)

	_, err = e.svc.CreateDraft(context.Background(), e.actor, content.FamilyScenarios, e.scenarioParent(), body, &[]uuid.UUID{uuid.New()}[0])
	require.Equal(t, errs.CodeNotFound, code(err), "copy source of another parent")
}

func TestCreateDraftRejectsBadBodies(t *testing.T) {
	e := newAdminEnv(t)
	for name, body := range map[string]string{
		"unknown field": `{"template":"x"}`,
		"wrong type":    `{"difficulty_level":"easy"}`,
		"not json":      `nope`,
	} {
		_, err := e.svc.CreateDraft(context.Background(), e.actor, content.FamilyScenarios, e.scenarioParent(), json.RawMessage(body), nil)
		require.Equal(t, errs.CodeValidationFailed, code(err), name)
	}
	_, err := e.svc.CreateDraft(context.Background(), e.actor, content.FamilyScenarios, e.scenarioParent(), json.RawMessage(`{"situation":"x"}`), nil)
	require.Equal(t, errs.CodeContentValidationFailed, code(err), "an incomplete draft without copy_from fails the content rules")
	require.Empty(t, e.store.Audit())
}

func TestCreateDraftPrompt(t *testing.T) {
	e := newAdminEnv(t)
	tmpl := validPrompt(content.PromptConversation).SystemTemplate
	b, _ := json.Marshal(map[string]any{"system_template": tmpl, "user_template": nil, "output_schema": nil, "llm_params": map[string]any{"temperature": 0.7}, "notes": "v2"})
	v, err := e.svc.CreateDraft(context.Background(), e.actor, content.FamilyPrompts, "CONVERSATION", b, nil)
	require.NoError(t, err)
	require.Equal(t, 2, v.Version)
	require.Equal(t, content.PromptConversation, v.Prompt.Key)
	require.InDelta(t, 0.7, *v.Prompt.LLMParams.Temperature, 1e-9)

	b, _ = json.Marshal(map[string]any{"system_template": tmpl, "llm_params": map[string]any{"model": "gpt"}})
	_, err = e.svc.CreateDraft(context.Background(), e.actor, content.FamilyPrompts, "CONVERSATION", b, nil)
	require.Equal(t, errs.CodeContentValidationFailed, code(err), "a model name does not belong in llm_params")
}

func TestListAndGetVersions(t *testing.T) {
	e := newAdminEnv(t)
	d := e.draftScenario(t, 2)
	list, err := e.svc.ListVersions(context.Background(), content.FamilyScenarios, e.scenarioParent())
	require.NoError(t, err)
	require.Len(t, list, 2)
	require.Equal(t, d.ID, list[0].ID, "version DESC")
	got, err := e.svc.GetVersion(context.Background(), content.FamilyScenarios, e.scenarioParent(), d.ID)
	require.NoError(t, err)
	require.Equal(t, d.ID, got.ID)
	_, err = e.svc.GetVersion(context.Background(), content.FamilyScenarios, e.scenarioParent(), uuid.New())
	require.Equal(t, errs.CodeNotFound, code(err))
	pl, err := e.svc.ListVersions(context.Background(), content.FamilyPrompts, "CONVERSATION")
	require.NoError(t, err)
	require.Len(t, pl, 1)
}

func TestSetStatusTopicAndScenarioOnly(t *testing.T) {
	e := newAdminEnv(t)
	v, err := e.svc.SetStatus(context.Background(), e.actor, "topics", e.w.topic.ID, content.StatusDisabled)
	require.NoError(t, err)
	require.Equal(t, content.StatusDisabled, v.Status)
	_, err = e.svc.SetStatus(context.Background(), e.actor, "scenarios", e.w.scenario.ID, content.StatusArchived)
	require.NoError(t, err)
	require.Equal(t, []string{content.ActionStatusChanged, content.ActionStatusChanged}, actions(e.store))
	require.Equal(t, content.TargetTopic, e.store.Audit()[0].TargetType)
	require.Equal(t, 2, e.inv.n)

	_, err = e.svc.SetStatus(context.Background(), e.actor, "topics", e.w.topic.ID, content.StatusDisabled)
	require.NoError(t, err, "same status: no-op")
	require.Len(t, e.store.Audit(), 2)

	_, err = e.svc.SetStatus(context.Background(), e.actor, "characters", e.w.character.ID, content.StatusDisabled)
	require.Equal(t, errs.CodeNotFound, code(err), "characters are edited with PATCH")
	_, err = e.svc.SetStatus(context.Background(), e.actor, "topics", e.w.topic.ID, content.EntityStatus("BOGUS"))
	require.Equal(t, errs.CodeValidationFailed, code(err))
	_, err = e.svc.SetStatus(context.Background(), e.actor, "topics", uuid.New(), content.StatusActive)
	require.Equal(t, errs.CodeNotFound, code(err))
}

func (e *adminEnv) character(t *testing.T, mod func(*content.Character)) content.Character {
	t.Helper()
	ch := e.w.character
	if mod != nil {
		mod(&ch)
	}
	e.w.repo.PutCharacter(ch)
	return ch
}

func str(s string) *string { return &s }

func TestUpdateCharacterPartialMerge(t *testing.T) {
	e := newAdminEnv(t)
	before := e.character(t, nil)
	e.clk.Advance(time.Minute)
	res, err := e.svc.UpdateCharacter(context.Background(), e.actor, before.ID, content.CharacterPatch{Persona: str(strings.Repeat("Persona baru. ", 5))})
	require.NoError(t, err)
	require.Equal(t, strings.Repeat("Persona baru. ", 5), res.Character.Persona)
	require.Equal(t, before.Name, res.Character.Name)
	require.Equal(t, before.Voice, res.Character.Voice)
	require.Equal(t, before.VoiceID, res.Character.VoiceID)
	got, _ := e.w.repo.GetCharacter(context.Background(), before.ID)
	require.Equal(t, res.Character.Persona, got.Persona)
	require.True(t, got.UpdatedAt.After(before.UpdatedAt))
	require.Equal(t, 1, e.inv.n)
}

func TestUpdateCharacterNoChangeWritesNothing(t *testing.T) {
	e := newAdminEnv(t)
	before := e.character(t, nil)
	_, err := e.svc.UpdateCharacter(context.Background(), e.actor, before.ID, content.CharacterPatch{Name: str(before.Name)})
	require.NoError(t, err)
	require.Empty(t, e.store.Audit())
	require.Zero(t, e.inv.n)
}

func TestUpdateCharacterVoiceReplacesWhole(t *testing.T) {
	e := newAdminEnv(t)
	ch := e.character(t, nil)
	require.NotNil(t, ch.Voice.Fallback)
	_, err := e.svc.UpdateCharacter(context.Background(), e.actor, ch.ID, content.CharacterPatch{
		Voice: &content.Voice{Gender: content.GenderMale, Accent: content.AccentKansai, SpeakingRate: 0.95}})
	require.NoError(t, err)
	got, _ := e.w.repo.GetCharacter(context.Background(), ch.ID)
	require.Equal(t, content.GenderMale, got.Voice.Gender)
	require.Nil(t, got.Voice.Fallback, "a fallback that was not sent is gone")
}

func TestUpdateCharacterProviderFields(t *testing.T) {
	e := newAdminEnv(t)
	ch := e.character(t, func(c *content.Character) { c.Voice.Fallback = nil })
	res, err := e.svc.UpdateCharacter(context.Background(), e.actor, ch.ID, content.CharacterPatch{
		ProviderName: str("cartesia"), ModelName: str("sonic3.6"), VoiceID: str("db6b0ed5")})
	require.NoError(t, err)
	require.Equal(t, "CARTESIA", res.Character.ProviderName)
	require.Equal(t, "sonic3.6", res.Character.ModelName)
	require.Equal(t, ch.Persona, res.Character.Persona)

	_, err = e.svc.UpdateCharacter(context.Background(), e.actor, ch.ID, content.CharacterPatch{ProviderName: str("AZURE")})
	requireIssue(t, err, "provider_name", "invalid_enum")

	_, err = e.svc.UpdateCharacter(context.Background(), e.actor, ch.ID, content.CharacterPatch{
		Voice: &content.Voice{Gender: content.GenderFemale, Accent: content.AccentKanto, SpeakingRate: 1,
			Fallback: &content.TTSBinding{ProviderName: "cartesia", ModelName: "m", VoiceID: "v"}}})
	requireIssue(t, err, "voice.fallback.provider_name", "fallback_same_provider")
}

func requireIssue(t *testing.T, err error, field, rule string) {
	t.Helper()
	require.Equal(t, errs.CodeContentValidationFailed, code(err), "%v", err)
	issues, _ := errs.DetailsOf(err).([]content.Issue)
	for _, is := range issues {
		if is.Field == field && is.Rule == rule {
			return
		}
	}
	require.Failf(t, "issue not found", "want %s/%s in %+v", field, rule, issues)
}

func TestUpdateCharacterPlaceholderVoiceInRealMode(t *testing.T) {
	e := newAdminEnv(t, func(d *content.AdminDeps) { d.Validator.TTSModeReal = true })
	ch := e.character(t, nil)
	_, err := e.svc.UpdateCharacter(context.Background(), e.actor, ch.ID, content.CharacterPatch{VoiceID: str("PLACEHOLDER_X")})
	requireIssue(t, err, "voice_id", "voice_placeholder")
}

func TestUpdateCharacterValidationError422(t *testing.T) {
	e := newAdminEnv(t)
	ch := e.character(t, nil)
	_, err := e.svc.UpdateCharacter(context.Background(), e.actor, ch.ID, content.CharacterPatch{
		Name: str("Changed"), Voice: &content.Voice{Gender: content.GenderFemale, Accent: content.AccentKanto, SpeakingRate: 1.5}})
	requireIssue(t, err, "voice.speaking_rate", "range")
	got, _ := e.w.repo.GetCharacter(context.Background(), ch.ID)
	require.Equal(t, ch, got, "rolled back, the row is unchanged")
	require.Empty(t, e.store.Audit(), "no audit entry on failure")
	require.Zero(t, e.inv.n)
}

func TestUpdateCharacterActivateWithoutConfiguredProvider422(t *testing.T) {
	e := newAdminEnv(t, func(d *content.AdminDeps) {
		d.Validator.TTSEnabled = func(p string) bool { return p != "ELEVENLABS" }
		d.Validator.TTSModeReal = true
	})
	ch := e.character(t, func(c *content.Character) { c.Status = content.StatusDisabled })
	_, err := e.svc.UpdateCharacter(context.Background(), e.actor, ch.ID, content.CharacterPatch{Status: &[]content.EntityStatus{content.StatusActive}[0]})
	requireIssue(t, err, "provider_name", "provider_not_configured")
}

func TestUpdateCharacterWarnings(t *testing.T) {
	e := newAdminEnv(t)
	ch := e.character(t, nil)
	res, err := e.svc.UpdateCharacter(context.Background(), e.actor, ch.ID, content.CharacterPatch{VoiceID: str("PLACEHOLDER_X")})
	require.NoError(t, err, "a placeholder is only a warning outside real mode")
	require.NotEmpty(t, res.Warnings)
	require.Equal(t, content.SeverityWarning, res.Warnings[0].Severity)
}

func TestUpdateCharacterAuditNoVoiceID(t *testing.T) {
	e := newAdminEnv(t)
	ch := e.character(t, func(c *content.Character) { c.Voice.Fallback = nil })
	_, err := e.svc.UpdateCharacter(context.Background(), e.actor, ch.ID, content.CharacterPatch{
		ProviderName: str("cartesia"), ModelName: str("secret-model"), VoiceID: str("secret-voice-id"), Name: str("Haruka-chan"),
		Voice: &content.Voice{Gender: content.GenderFemale, Accent: content.AccentKanto, SpeakingRate: 1,
			Fallback: &content.TTSBinding{ProviderName: "ELEVENLABS", ModelName: "secret-fallback", VoiceID: "secret-fb-voice"}}})
	require.NoError(t, err)
	log := e.store.Audit()
	require.Len(t, log, 1)
	require.Equal(t, content.ActionCharacterUpdated, log[0].Action)
	require.Equal(t, content.TargetCharacter, log[0].TargetType)
	require.Equal(t, ch.ID.String(), log[0].TargetID)
	require.ElementsMatch(t, []string{"name", "provider_name", "model_name", "voice_id", "voice"}, log[0].Details["changed_fields"])
	require.Equal(t, "ELEVENLABS", log[0].Details["provider_name_before"])
	require.Equal(t, "CARTESIA", log[0].Details["provider_name_after"])
	raw, _ := json.Marshal(log[0])
	for _, secret := range []string{"secret-model", "secret-voice-id", "secret-fallback", "secret-fb-voice"} {
		require.NotContains(t, string(raw), secret)
	}

	_, err = e.svc.UpdateCharacter(context.Background(), e.actor, ch.ID, content.CharacterPatch{Persona: str("short")})
	require.Error(t, err)
	require.Len(t, e.store.Audit(), 1)
}

func TestUpdateCharacterUnknownID(t *testing.T) {
	e := newAdminEnv(t)
	_, err := e.svc.UpdateCharacter(context.Background(), e.actor, uuid.New(), content.CharacterPatch{Name: str("x")})
	require.Equal(t, errs.CodeNotFound, code(err))
}

func TestUpdateCharacterInvalidatesCache(t *testing.T) {
	registerFixtures(t)
	w := newWorld()
	w.put()
	cached := content.NewCachedRepository(w.repo, time.Hour, clock.NewFake(t0))
	resolver := content.NewResolver(cached, realCfg(), nil)
	svc := content.NewAdminService(content.AdminDeps{Store: fakes.NewContentStore(w.repo), Clock: clock.NewFake(t0), IDs: idgen.NewV7(),
		Invalidator: cached.(content.Invalidator), Validator: content.ValidatorConfig{}})

	first, err := resolver.Resolve(context.Background(), w.input())
	require.NoError(t, err)
	newPersona := strings.Repeat("Persona baru yang berbeda. ", 3)
	_, err = svc.UpdateCharacter(context.Background(), uuid.New(), w.character.ID, content.CharacterPatch{Persona: &newPersona})
	require.NoError(t, err)
	second, err := resolver.Resolve(context.Background(), w.input())
	require.NoError(t, err)
	require.NotEqual(t, first.ConfigSnapshot.Character.Persona, second.ConfigSnapshot.Character.Persona)
	require.Equal(t, newPersona, second.ConfigSnapshot.Character.Persona)
}

func TestValidationReport(t *testing.T) {
	e := newAdminEnv(t)
	errsOut, warns, err := e.svc.ValidationReport(context.Background())
	require.NoError(t, err)
	require.NotNil(t, errsOut)
	require.NotNil(t, warns)
	require.NotEmpty(t, warns, "the other prompt keys have no PUBLISHED version yet")
}

func TestGetCharacterAdminView(t *testing.T) {
	e := newAdminEnv(t)
	ch, err := e.svc.GetCharacter(context.Background(), e.w.character.ID)
	require.NoError(t, err)
	require.Equal(t, e.w.character.VoiceID, ch.VoiceID)
	_, err = e.svc.GetCharacter(context.Background(), uuid.New())
	require.Equal(t, errs.CodeNotFound, code(err))
}
