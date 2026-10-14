//go:build integration || e2e

package testutil

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// ContentIDs identifies one published row per versioned family plus one active character.
type ContentIDs struct {
	TopicID, ScenarioID, ScenarioVersionID, CharacterID, PromptVersionID uuid.UUID
}

// ValidVoiceJSON returns the minimal valid characters.voice object (D-015), with
// per-key overrides applied.
func ValidVoiceJSON(o ...map[string]any) []byte {
	v := map[string]any{"gender": "FEMALE", "accent": "KANTO", "speaking_rate": 1.0}
	for _, m := range o {
		for k, val := range m {
			v[k] = val
		}
	}
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

func newID() uuid.UUID { return uuid.Must(uuid.NewV7()) }

func short(id uuid.UUID) string { return strings.ToUpper(strings.ReplaceAll(id.String(), "-", ""))[20:] }

// TopicRow returns a valid topics row.
func TopicRow(o Row) Row {
	id := newID()
	return merge(Row{"id": id, "code": "T_" + short(id), "name": "Topic", "name_ja": "トピック"}, o)
}

// ScenarioRow returns a valid scenarios row under topicID.
func ScenarioRow(topicID uuid.UUID, o Row) Row {
	id := newID()
	return merge(Row{"id": id, "topic_id": topicID, "code": "S_" + short(id), "name": "Scenario", "name_ja": "シナリオ"}, o)
}

// ScenarioVersionRow returns a valid DRAFT scenario_versions row.
func ScenarioVersionRow(scenarioID uuid.UUID, version int, o Row) Row {
	return merge(Row{
		"id": newID(), "scenario_id": scenarioID, "version": version,
		"difficulty_level": 2, "recommended_level_min": 1.0, "recommended_level_max": 3.0,
		"situation": "Di kafe", "ai_role": "Barista", "user_role": "Pelanggan", "goal": "Memesan kopi",
		"opening_line": "いらっしゃいませ。", "visual_prompt": []byte(`{}`),
	}, o)
}

// CharacterRow returns a valid ACTIVE characters row.
func CharacterRow(o Row) Row {
	id := newID()
	return merge(Row{
		"id": id, "code": "C_" + short(id), "name": "Haruka",
		"persona":       strings.Repeat("Ramah dan sabar. ", 5),
		"provider_name": "ELEVENLABS", "model_name": "PLACEHOLDER_MODEL_TEST", "voice_id": "PLACEHOLDER_VOICE_TEST",
		"voice": ValidVoiceJSON(),
	}, o)
}

// PromptConfigRow returns a prompt_configs row for key.
func PromptConfigRow(key string, o Row) Row {
	return merge(Row{"id": newID(), "key": key}, o)
}

// PromptVersionRow returns a valid DRAFT prompt_versions row.
func PromptVersionRow(configID uuid.UUID, version int, o Row) Row {
	return merge(Row{"id": newID(), "prompt_config_id": configID, "version": version, "system_template": "Kamu adalah {{.Name}}."}, o)
}

// SeedContent inserts one PUBLISHED row per versioned family and an ACTIVE character.
func SeedContent(t testing.TB, db DBTX) ContentIDs {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	topic := TopicRow(nil)
	MustInsert(t, ctx, db, "topics", topic)
	sc := ScenarioRow(topic["id"].(uuid.UUID), nil)
	MustInsert(t, ctx, db, "scenarios", sc)
	sv := ScenarioVersionRow(sc["id"].(uuid.UUID), 1, Row{"status": "PUBLISHED", "published_at": now})
	MustInsert(t, ctx, db, "scenario_versions", sv)
	ch := CharacterRow(nil)
	MustInsert(t, ctx, db, "characters", ch)
	pc := PromptConfigRow("CONVERSATION", nil)
	MustInsert(t, ctx, db, "prompt_configs", pc)
	pv := PromptVersionRow(pc["id"].(uuid.UUID), 1, Row{"status": "PUBLISHED", "published_at": now})
	MustInsert(t, ctx, db, "prompt_versions", pv)
	return ContentIDs{
		TopicID: topic["id"].(uuid.UUID), ScenarioID: sc["id"].(uuid.UUID), ScenarioVersionID: sv["id"].(uuid.UUID),
		CharacterID: ch["id"].(uuid.UUID), PromptVersionID: pv["id"].(uuid.UUID),
	}
}
