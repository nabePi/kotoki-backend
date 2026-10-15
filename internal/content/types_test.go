package content_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/content"
)

func TestScenarioFormality(t *testing.T) {
	require.Equal(t, content.FormalityKeigo, content.ScenarioFormality(content.ScenarioVersion{Formality: content.FormalityKeigo}))
	require.Equal(t, content.FormalityCasual, content.ScenarioFormality(content.ScenarioVersion{Formality: content.FormalityCasual}))
}

func TestSpeakingRate(t *testing.T) {
	require.InDelta(t, 0.95, content.SpeakingRate(1.00, 0.95), 1e-9)
	require.InDelta(t, 0.9025, content.SpeakingRate(0.95, 0.95), 1e-9)
	require.Equal(t, 1.0, content.SpeakingRate(1.0, 0), "adaptive 0 means 1.0")
	require.Equal(t, 0.95, content.SpeakingRate(0.95, -1))
	require.Equal(t, 0.80, content.SpeakingRate(0.80, 0.9), "clamped to the floor")
	require.Equal(t, 1.05, content.SpeakingRate(1.20, 1.2), "clamped to the ceiling")
	require.Equal(t, 1.05, content.SpeakingRate(1.05, 1.0))
}

func TestDifficultyBand(t *testing.T) {
	for level, want := range map[int][2]string{1: {"BEGINNER", "N5"}, 2: {"ELEMENTARY", "N4"}, 3: {"INTERMEDIATE", "N3"},
		4: {"UPPER_INTERMEDIATE", "N2"}, 5: {"ADVANCED", "N1"}} {
		band, jlpt := content.DifficultyBand(level)
		require.Equal(t, want[0], band, level)
		require.Equal(t, want[1], jlpt, level)
	}
	for _, bad := range []int{0, 6, -1} {
		band, jlpt := content.DifficultyBand(bad)
		require.Empty(t, band)
		require.Empty(t, jlpt)
	}
}

func TestPromptVersionTemplate(t *testing.T) {
	pv := content.PromptVersion{Key: content.PromptConversation, Version: 3, SystemTemplate: "sys"}
	tpl := pv.Template()
	require.Equal(t, "CONVERSATION", tpl.Key)
	require.Equal(t, 3, tpl.Version)
	require.Equal(t, "sys", tpl.System)
	require.Empty(t, tpl.User, "a nil user template is an empty User")
	user := "usr {{.X}}"
	pv.UserTemplate = &user
	require.Equal(t, user, pv.Template().User)
}

func TestDefaultStyle(t *testing.T) {
	require.Equal(t, 3, content.DefaultStyle.MaxSentencesPerTurn)
	require.Equal(t, 2, content.DefaultStyle.MaxConsecutiveQuestions)
}

func snapshot(fallback bool) content.SessionSnapshot {
	greeting := "やあ"
	s := content.SessionSnapshot{
		Formality: content.FormalityPolite,
		Character: content.CharacterSnapshot{
			ID: uuid.MustParse("0192f1a0-1111-7000-8000-000000000001"), Code: "HARUKA", Name: "Haruka", Persona: "Friendly and patient.",
			ProviderName: "ELEVENLABS", ModelName: "Eleven v3 Conversational", VoiceID: "voice-1",
			Voice: content.Voice{Gender: content.GenderFemale, Accent: content.AccentKanto, SpeakingRate: 1.0},
		},
	}
	if fallback {
		s.Character.GreetingHint = &greeting
		s.Character.Voice.Fallback = &content.TTSBinding{ProviderName: "CARTESIA", ModelName: "sonic3.6", VoiceID: "voice-2"}
	}
	return s
}

func TestSnapshotRoundTrip(t *testing.T) {
	for _, fb := range []bool{false, true} {
		in := snapshot(fb)
		raw, err := in.MarshalJSONB()
		require.NoError(t, err)
		out, err := content.ParseSnapshot(raw)
		require.NoError(t, err)
		require.Equal(t, in, out)

		var doc map[string]any
		require.NoError(t, json.Unmarshal(raw, &doc))
		ch := doc["character"].(map[string]any)
		require.Equal(t, "ELEVENLABS", ch["provider_name"])
		require.Equal(t, "Eleven v3 Conversational", ch["model_name"])
		require.Equal(t, "voice-1", ch["voice_id"])
		require.Equal(t, "POLITE", doc["formality"])
		voice := ch["voice"].(map[string]any)
		if fb {
			require.Contains(t, voice, "fallback")
			require.Contains(t, ch, "greeting_hint")
		} else {
			require.NotContains(t, voice, "fallback", "no fallback → the key is absent")
			require.NotContains(t, ch, "greeting_hint", "a nil greeting hint is omitted")
			require.NotContains(t, ch, "avatar_key")
		}
	}
}

func TestSnapshotRejectsUnknownKeys(t *testing.T) {
	raw, err := snapshot(false).MarshalJSONB()
	require.NoError(t, err)
	for name, mutated := range map[string]string{
		"top level":       strings.Replace(string(raw), `"formality"`, `"extra":1,"formality"`, 1),
		"character level": strings.Replace(string(raw), `"persona"`, `"mood":"x","persona"`, 1),
		"voice providers": strings.Replace(string(raw), `"speaking_rate"`, `"providers":{},"speaking_rate"`, 1),
	} {
		_, err := content.ParseSnapshot([]byte(mutated))
		require.Error(t, err, name)
	}
	_, err = content.ParseSnapshot([]byte(`not json`))
	require.Error(t, err)
}

func TestTTSBindings(t *testing.T) {
	c := snapshot(true).Character
	require.Equal(t, content.TTSBinding{ProviderName: "ELEVENLABS", ModelName: "Eleven v3 Conversational", VoiceID: "voice-1"}, c.PrimaryTTS())
	fb, ok := c.FallbackTTS()
	require.True(t, ok)
	require.Equal(t, content.TTSBinding{ProviderName: "CARTESIA", ModelName: "sonic3.6", VoiceID: "voice-2"}, fb)
	_, ok = snapshot(false).Character.FallbackTTS()
	require.False(t, ok)
}

func TestNormalizeProviderName(t *testing.T) {
	require.Equal(t, "CARTESIA", content.NormalizeProviderName(" cartesia "))
	require.Equal(t, "ELEVENLABS", content.NormalizeProviderName("ElevenLabs"))
	require.Empty(t, content.NormalizeProviderName("  "))
}
