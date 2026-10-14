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
	"github.com/nabePi/kotoki-backend/internal/testutil/fakes"
)

var t0 = time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)

type world struct {
	repo      *fakes.ContentRepo
	topic     content.Topic
	scenario  content.Scenario
	sv        content.ScenarioVersion
	character content.Character
	prompt    content.PromptVersion
}

func newWorld() *world {
	w := &world{repo: fakes.NewContentRepo()}
	w.topic = content.Topic{ID: uuid.New(), Code: "INTERVIEW", Name: "Interview", NameJA: "面接", Status: content.StatusActive}
	w.scenario = content.Scenario{ID: uuid.New(), TopicID: w.topic.ID, Code: "BACKEND_ENGINEER_INTERVIEW", Name: "Backend engineer interview", Status: content.StatusActive}
	w.sv = validScenario()
	w.sv.ID, w.sv.ScenarioID, w.sv.Status, w.sv.Formality = uuid.New(), w.scenario.ID, content.VersionPublished, content.FormalityKeigo
	w.character = validCharacter()
	w.prompt = validPromptRow()
	w.put()
	return w
}

func validPromptRow() content.PromptVersion {
	return content.PromptVersion{ID: uuid.New(), Key: content.PromptConversation, Version: 1, Status: content.VersionPublished, SystemTemplate: "x"}
}

func (w *world) put() {
	w.repo.PutTopic(w.topic)
	w.repo.PutScenario(w.scenario)
	w.repo.PutScenarioVersion(w.sv)
	w.repo.PutCharacter(w.character)
	w.repo.PutPromptVersion(w.prompt)
}

func (w *world) input() content.ResolveInput {
	return content.ResolveInput{UserID: uuid.New(), TopicID: w.topic.ID, ScenarioID: w.scenario.ID, CharacterID: w.character.ID, ConversationStartMode: "USER_FIRST"}
}

type obsLog struct {
	resolve  []string
	fallback []string
}

func (o *obsLog) ResolveError(c string)         { o.resolve = append(o.resolve, c) }
func (o *obsLog) VoiceFallbackMissing(c string) { o.fallback = append(o.fallback, c) }
func (o *obsLog) ValidationIssues(string, int)  {}

func realCfg() content.ResolverConfig {
	return content.ResolverConfig{TTSMode: "real", TTSEnabled: func(string) bool { return true }, LLMConfigured: true}
}

func fakeCfg() content.ResolverConfig {
	c := realCfg()
	c.TTSMode = "fake"
	return c
}

func TestResolve(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*world)
		cfg    content.ResolverConfig
		code   errs.Code // "" = success
	}{
		{"ok", func(*world) {}, realCfg(), ""},
		{"topic missing", func(w *world) { w.topic.ID = uuid.New() }, realCfg(), errs.CodeTopicNotAvailable},
		{"topic disabled", func(w *world) { w.topic.Status = content.StatusDisabled; w.put() }, realCfg(), errs.CodeTopicNotAvailable},
		{"topic archived", func(w *world) { w.topic.Status = content.StatusArchived; w.put() }, realCfg(), errs.CodeTopicNotAvailable},
		{"scenario missing", func(w *world) { w.scenario.ID = uuid.New() }, realCfg(), errs.CodeScenarioNotAvailable},
		{"scenario disabled", func(w *world) { w.scenario.Status = content.StatusDisabled; w.put() }, realCfg(), errs.CodeScenarioNotAvailable},
		{"scenario without a published version", func(w *world) { w.sv.Status = content.VersionDraft; w.put() }, realCfg(), errs.CodeScenarioNotAvailable},
		{"scenario of another topic", func(w *world) { w.scenario.TopicID = uuid.New(); w.put() }, realCfg(), errs.CodeScenarioTopicMismatch},
		{"character missing", func(w *world) { w.character.ID = uuid.New() }, realCfg(), errs.CodeCharacterNotAvailable},
		{"character disabled", func(w *world) { w.character.Status = content.StatusDisabled; w.put() }, realCfg(), errs.CodeCharacterNotAvailable},
		{"character archived", func(w *world) { w.character.Status = content.StatusArchived; w.put() }, realCfg(), errs.CodeCharacterNotAvailable},
		{"vendor not enabled (real)", func(w *world) { w.character.ProviderName = "CARTESIA"; w.put() }, withTTS(realCfg(), "ELEVENLABS"), errs.CodeCharacterNotAvailable},
		{"vendor not enabled (fake)", func(w *world) { w.character.ProviderName = "CARTESIA"; w.put() }, withTTS(fakeCfg(), "ELEVENLABS"), errs.CodeCharacterNotAvailable},
		{"real: empty model", func(w *world) { w.character.ModelName = ""; w.put() }, realCfg(), errs.CodeContentMisconfigured},
		{"real: empty voice id", func(w *world) { w.character.VoiceID = " "; w.put() }, realCfg(), errs.CodeContentMisconfigured},
		{"real: placeholder voice id", func(w *world) { w.character.VoiceID = "PLACEHOLDER_VOICE_HARUKA"; w.put() }, realCfg(), errs.CodeContentMisconfigured},
		{"fake: placeholder voice id is fine", func(w *world) { w.character.VoiceID = "PLACEHOLDER_VOICE_HARUKA"; w.put() }, fakeCfg(), ""},
		{"fake: empty model is fine", func(w *world) { w.character.ModelName = ""; w.put() }, fakeCfg(), ""},
		{"no conversation prompt", func(w *world) { w.prompt.Status = content.VersionDraft; w.put() }, realCfg(), errs.CodeContentMisconfigured},
		{"no LLM model", func(*world) {}, func() content.ResolverConfig { c := realCfg(); c.LLMConfigured = false; return c }(), errs.CodeContentMisconfigured},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := newWorld()
			c.mutate(w)
			obs := &obsLog{}
			got, err := content.NewResolver(w.repo, c.cfg, obs).Resolve(context.Background(), w.input())
			if c.code == "" {
				require.NoError(t, err)
				require.Equal(t, w.sv.ID, got.ScenarioVersionID)
				require.Equal(t, w.prompt.ID, got.PromptVersionID)
				require.Empty(t, obs.resolve, "a success is not counted as an error")
				return
			}
			require.Error(t, err)
			require.Equal(t, c.code, errs.CodeOf(err))
			require.Equal(t, []string{string(c.code)}, obs.resolve, "each returned error is counted exactly once")
		})
	}
}

func withTTS(c content.ResolverConfig, enabled ...string) content.ResolverConfig {
	c.TTSEnabled = func(p string) bool {
		for _, e := range enabled {
			if e == p {
				return true
			}
		}
		return false
	}
	return c
}

func TestResolve_OrderTopicFirst(t *testing.T) {
	w := newWorld()
	w.topic.Status = content.StatusDisabled
	w.scenario.Status = content.StatusDisabled
	w.character.Status = content.StatusDisabled
	w.put()
	_, err := content.NewResolver(w.repo, realCfg(), nil).Resolve(context.Background(), w.input())
	require.Equal(t, errs.CodeTopicNotAvailable, errs.CodeOf(err))
}

func TestResolve_AIFirstWithoutOpeningSucceeds(t *testing.T) {
	w := newWorld() // no OPENING prompt exists at all
	in := w.input()
	in.ConversationStartMode = "AI_FIRST"
	_, err := content.NewResolver(w.repo, realCfg(), nil).Resolve(context.Background(), in)
	require.NoError(t, err, "OPENING falls back to opening_line later (S-2)")
}

func TestResolve_VoiceFallbackMissingIsAWarning(t *testing.T) {
	for name, mutate := range map[string]func(*content.Character){
		"nil":           func(c *content.Character) { c.Voice.Fallback = nil },
		"placeholder":   func(c *content.Character) { c.Voice.Fallback.VoiceID = "PLACEHOLDER_X" },
		"vendor not on": func(c *content.Character) { c.Voice.Fallback.ProviderName = "CARTESIA" },
	} {
		t.Run(name, func(t *testing.T) {
			w := newWorld()
			mutate(&w.character)
			w.put()
			obs := &obsLog{}
			cfg := withTTS(realCfg(), "ELEVENLABS")
			_, err := content.NewResolver(w.repo, cfg, obs).Resolve(context.Background(), w.input())
			require.NoError(t, err, "a missing fallback never blocks")
			require.Equal(t, []string{"HARUKA"}, obs.fallback)
		})
	}
	w := newWorld()
	obs := &obsLog{}
	_, err := content.NewResolver(w.repo, realCfg(), obs).Resolve(context.Background(), w.input())
	require.NoError(t, err)
	require.Empty(t, obs.fallback, "a usable fallback is not reported")

	w.character.Voice.Fallback = nil
	w.put()
	obs = &obsLog{}
	_, err = content.NewResolver(w.repo, fakeCfg(), obs).Resolve(context.Background(), w.input())
	require.NoError(t, err)
	require.Empty(t, obs.fallback, "fake mode does not inspect the fallback")
}

func TestResolve_Snapshot(t *testing.T) {
	w := newWorld()
	greeting, avatar := "やあ", "haruka"
	w.character.GreetingHint, w.character.AvatarKey = &greeting, &avatar
	w.put()
	cfg := realCfg()
	got, err := content.NewResolver(w.repo, cfg, nil).Resolve(context.Background(), w.input())
	require.NoError(t, err)

	ch := got.ConfigSnapshot.Character
	require.Equal(t, w.character.ID, ch.ID)
	require.Equal(t, "HARUKA", ch.Code)
	require.Equal(t, "Haruka", ch.Name)
	require.Equal(t, w.character.Persona, ch.Persona)
	require.Equal(t, "やあ", *ch.GreetingHint)
	require.Equal(t, "haruka", *ch.AvatarKey)
	require.Equal(t, "ELEVENLABS", ch.ProviderName)
	require.Equal(t, "Eleven v3 Conversational", ch.ModelName)
	require.Equal(t, "voice-1", ch.VoiceID)
	require.Equal(t, content.GenderFemale, ch.Voice.Gender)
	require.Equal(t, content.AccentKanto, ch.Voice.Accent)
	require.Equal(t, 1.0, ch.Voice.SpeakingRate)
	require.Equal(t, "CARTESIA", ch.Voice.Fallback.ProviderName)
	require.Equal(t, content.FormalityKeigo, got.ConfigSnapshot.Formality, "the scenario's base formality")

	raw, err := got.ConfigSnapshot.MarshalJSONB()
	require.NoError(t, err)
	for _, banned := range []string{"provider_profile", "style", "expression", "voice_profile", "tier"} {
		require.NotContains(t, string(raw), banned)
	}

	// editing the live character later never reaches a snapshot already taken
	before := got.ConfigSnapshot
	w.character.Persona = "Completely new persona. " + strings.Repeat("x", 40)
	w.character.Voice.Fallback.VoiceID = "voice-changed"
	w.put()
	require.Equal(t, "voice-2", before.Character.Voice.Fallback.VoiceID, "the fallback pointer was deep-copied")
	require.NotContains(t, before.Character.Persona, "Completely new")

	clk := clock.NewFake(t0)
	cached := content.NewCachedRepository(w.repo, 30*time.Second, clk)
	again, err := content.NewResolver(cached, cfg, nil).Resolve(context.Background(), w.input())
	require.NoError(t, err)
	require.Contains(t, again.ConfigSnapshot.Character.Persona, "Completely new", "a new session sees the edit")
	require.NotContains(t, before.Character.Persona, "Completely new")
}

func TestResolvedConfigHasNoProviderProfileFields(t *testing.T) {
	raw, err := json.Marshal(content.ResolvedConfig{})
	require.NoError(t, err)
	for _, banned := range []string{"ProviderProfile", "Style", "Expression", "VoiceProfile", "Tier"} {
		require.NotContains(t, string(raw), banned)
	}
	raw, err = json.Marshal(content.SessionConfig{})
	require.NoError(t, err)
	require.NotContains(t, string(raw), "ProviderProfile")
}

func TestResolve_InternalErrorOnRepoFailure(t *testing.T) {
	w := newWorld()
	w.repo.FailNext("GetTopic", errBoom)
	_, err := content.NewResolver(w.repo, realCfg(), nil).Resolve(context.Background(), w.input())
	require.Equal(t, errs.CodeInternalError, errs.CodeOf(err))
}

var errBoom = &boom{}

type boom struct{}

func (*boom) Error() string { return "boom" }
