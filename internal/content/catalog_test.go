package content_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/content"
	"github.com/nabePi/kotoki-backend/internal/platform/errs"
)

func TestCatalogTopicsNeedAUsableScenario(t *testing.T) {
	w := newWorld()
	empty := content.Topic{ID: uuid.New(), Code: "EMPTY", Name: "Empty", Status: content.StatusActive}
	w.repo.PutTopic(empty)
	c := content.NewCatalogService(w.repo, realCfg())
	topics, err := c.Topics(context.Background())
	require.NoError(t, err)
	require.Len(t, topics, 1)
	require.Equal(t, w.topic.ID, topics[0].ID)
	require.Equal(t, 1, topics[0].ScenarioCount)

	w.sv.Status = content.VersionRetired
	w.put()
	topics, err = c.Topics(context.Background())
	require.NoError(t, err)
	require.Empty(t, topics, "no published version: the topic disappears")
}

func TestCatalogScenarios(t *testing.T) {
	w := newWorld()
	c := content.NewCatalogService(w.repo, realCfg())
	list, err := c.Scenarios(context.Background(), nil)
	require.NoError(t, err)
	require.Len(t, list, 1)
	v := list[0]
	require.Equal(t, w.sv.ID, v.VersionID)
	require.Equal(t, "ELEMENTARY", v.DifficultyBand)
	require.Equal(t, "N4", v.JLPTLabel)
	require.Equal(t, content.FormalityKeigo, v.Formality)

	other := uuid.New()
	list, err = c.Scenarios(context.Background(), &other)
	require.NoError(t, err)
	require.NotNil(t, list)
	require.Empty(t, list)

	one, err := c.Scenario(context.Background(), w.scenario.ID)
	require.NoError(t, err)
	require.Equal(t, v, one)

	w.topic.Status = content.StatusDisabled
	w.put()
	_, err = c.Scenario(context.Background(), w.scenario.ID)
	require.Equal(t, errs.CodeNotFound, errs.CodeOf(err), "disabled topic hides its scenarios")
	_, err = c.Scenario(context.Background(), uuid.New())
	require.Equal(t, errs.CodeNotFound, errs.CodeOf(err))
}

func TestCharacterHiddenWhenDisabled(t *testing.T) {
	w := newWorld()
	c := content.NewCatalogService(w.repo, realCfg())
	list, err := c.Characters(context.Background())
	require.NoError(t, err)
	require.Len(t, list, 1)

	w.character.Status = content.StatusDisabled
	w.put()
	list, err = c.Characters(context.Background())
	require.NoError(t, err)
	require.Empty(t, list)
	_, err = c.Character(context.Background(), w.character.ID)
	require.Equal(t, errs.CodeNotFound, errs.CodeOf(err))
}

func TestCharacterHiddenWhenProviderNotConfiguredRealMode(t *testing.T) {
	w := newWorld()
	off := func(p string) bool { return p != w.character.ProviderName }
	for _, mode := range []string{"real", "fake"} {
		cfg := realCfg()
		cfg.TTSMode, cfg.TTSEnabled = mode, off
		list, err := content.NewCatalogService(w.repo, cfg).Characters(context.Background())
		require.NoError(t, err)
		require.Empty(t, list, "a vendor that is switched off hides the character in "+mode+" mode")
	}

	w.character.VoiceID = "PLACEHOLDER_X"
	w.put()
	list, err := content.NewCatalogService(w.repo, realCfg()).Characters(context.Background())
	require.NoError(t, err)
	require.Empty(t, list, "placeholder voice in real mode")
	list, err = content.NewCatalogService(w.repo, fakeCfg()).Characters(context.Background())
	require.NoError(t, err)
	require.Len(t, list, 1, "fake mode does not look at the voice id")
}

func TestCharacterViewGenderAccentFromVoice(t *testing.T) {
	w := newWorld()
	w.character.Voice.Gender, w.character.Voice.Accent = content.GenderMale, content.AccentKansai
	w.put()
	v, err := content.NewCatalogService(w.repo, realCfg()).Character(context.Background(), w.character.ID)
	require.NoError(t, err)
	require.Equal(t, content.GenderMale, v.Gender)
	require.Equal(t, content.AccentKansai, v.Accent)
}

func TestCatalogOmitsPrivateFields(t *testing.T) {
	w := newWorld()
	c := content.NewCatalogService(w.repo, realCfg())
	sc, err := c.Scenarios(context.Background(), nil)
	require.NoError(t, err)
	ch, err := c.Characters(context.Background())
	require.NoError(t, err)
	raw, err := json.Marshal(map[string]any{"scenarios": sc, "characters": ch})
	require.NoError(t, err)
	body := string(raw)
	for _, key := range []string{"persona", "voice", "provider_name", "model_name", "voice_id", "greeting_hint", "speaking_rate", "opening_line", "constraints", "fallback"} {
		require.NotContains(t, body, `"`+key+`"`)
	}
	require.False(t, strings.Contains(body, w.character.Persona))
	require.NotContains(t, body, w.sv.OpeningLine)
}
