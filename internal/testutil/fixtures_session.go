//go:build integration || e2e

package testutil

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
)

// SessionRow returns a valid RESERVED sessions row built from seeded content.
func SessionRow(userID uuid.UUID, c ContentIDs, o Row) Row {
	snap, err := json.Marshal(map[string]any{
		"character": map[string]any{
			"id": c.CharacterID, "code": "HARUKA", "name": "Haruka", "avatar_key": nil,
			"persona": "Ramah dan sabar.", "greeting_hint": nil,
			"provider_name": "ELEVENLABS", "model_name": "PLACEHOLDER_MODEL_TEST", "voice_id": "PLACEHOLDER_VOICE_TEST",
			"voice": map[string]any{"gender": "FEMALE", "accent": "KANTO", "speaking_rate": 1.0},
		},
		"formality": "POLITE",
	})
	if err != nil {
		panic(err)
	}
	id := newID()
	return merge(Row{
		"id": id, "user_id": userID, "status": "RESERVED", "conversation_start_mode": "USER_FIRST",
		"duration_seconds": 300, "reserved_seconds": 300,
		"topic_id": c.TopicID, "scenario_id": c.ScenarioID, "scenario_version_id": c.ScenarioVersionID,
		"character_id": c.CharacterID, "prompt_version_id": c.PromptVersionID,
		"config_snapshot": snap, "adaptive_difficulty": []byte(`{}`),
		"create_idempotency_key": "idem-" + id.String(), "create_request_hash": make([]byte, 32),
	}, o)
}

// SeedSession inserts a session and returns its id.
func SeedSession(t testing.TB, db DBTX, userID uuid.UUID, c ContentIDs, o Row) uuid.UUID {
	t.Helper()
	r := SessionRow(userID, c, o)
	MustInsert(t, context.Background(), db, "sessions", r)
	return r["id"].(uuid.UUID)
}
