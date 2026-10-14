//go:build integration

package integration_test

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/content"
	"github.com/nabePi/kotoki-backend/internal/platform/clock"
	"github.com/nabePi/kotoki-backend/internal/platform/idgen"
	"github.com/nabePi/kotoki-backend/internal/postgres"
	"github.com/nabePi/kotoki-backend/internal/prompt"
	"github.com/nabePi/kotoki-backend/internal/testutil"
)

type contentEnv struct {
	pool  *pgxpool.Pool
	store *postgres.Store
	svc   *content.AdminService
	ids   testutil.ContentIDs
	admin uuid.UUID
}

func newContentEnv(t *testing.T) *contentEnv {
	t.Helper()
	pool := testutil.NewPostgres(t)
	ids := idgen.NewV7()
	store := postgres.NewStore(pool, postgres.WithIDs(ids), postgres.WithClock(clock.System{}))
	svc := content.NewAdminService(content.AdminDeps{
		Store: store.ContentStore(), Clock: clock.System{}, IDs: ids,
		Validator: content.ValidatorConfig{TTSEnabled: func(string) bool { return true }},
		RequestID: func(context.Context) string { return "req-it" },
	})
	return &contentEnv{pool: pool, store: store, svc: svc, ids: testutil.SeedContent(t, pool), admin: testutil.SeedUser(t, pool, nil)}
}

const scenarioBodyJSON = `{
 "difficulty_level": 2, "recommended_level_min": 1.0, "recommended_level_max": 2.5,
 "situation": "You order lunch at a small restaurant.", "ai_role": "A waiter", "user_role": "A customer", "goal": "Order a meal",
 "tasks": ["order food"], "constraints": ["one question at a time"], "grammar_targets": ["〜をください"],
 "vocab_hints": [{"term_ja":"注文","reading":"ちゅうもん","meaning":"order"}], "evaluation_focus": ["GRAMMAR"],
 "formality": "POLITE", "opening_line": "いらっしゃいませ。",
 "visual_prompt": {"headline":"Lunch","body":"Order something","starter_phrases_ja":["すみません"],"image_key":null}}`

func TestPublishConcurrent(t *testing.T) {
	e := newContentEnv(t)
	ctx := context.Background()
	parent := e.ids.ScenarioID.String()
	var drafts [2]uuid.UUID
	for i := range drafts {
		v, err := e.svc.CreateDraft(ctx, e.admin, content.FamilyScenarios, parent, json.RawMessage(scenarioBodyJSON), nil)
		require.NoError(t, err)
		drafts[i] = v.ID
	}
	var wg sync.WaitGroup
	errc := make(chan error, len(drafts))
	for _, id := range drafts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := e.svc.Publish(ctx, e.admin, content.FamilyScenarios, parent, id)
			errc <- err
		}()
	}
	wg.Wait()
	close(errc)
	for err := range errc {
		require.NoError(t, err, "serialised by the parent lock: no 500 and no leaked 23505")
	}
	var published int
	require.NoError(t, e.pool.QueryRow(ctx, `SELECT count(*) FROM scenario_versions WHERE scenario_id = $1 AND status = 'PUBLISHED'`, e.ids.ScenarioID).Scan(&published))
	require.Equal(t, 1, published)
	var audits int
	require.NoError(t, e.pool.QueryRow(ctx, `SELECT count(*) FROM admin_audit_logs WHERE action = 'CONTENT_VERSION_PUBLISHED'`).Scan(&audits))
	require.Equal(t, 2, audits)
}

func TestPublishPromptAndRetirePreviousInPostgres(t *testing.T) {
	e := newContentEnv(t)
	ctx := context.Background()
	body, _ := json.Marshal(map[string]any{"system_template": prompt.ConversationV1.System, "llm_params": map[string]any{"temperature": 0.7}, "notes": "tuned"})
	d, err := e.svc.CreateDraft(ctx, e.admin, content.FamilyPrompts, "CONVERSATION", body, nil)
	require.NoError(t, err)
	res, err := e.svc.Publish(ctx, e.admin, content.FamilyPrompts, "CONVERSATION", d.ID)
	require.NoError(t, err)
	require.Equal(t, e.ids.PromptVersionID, *res.RetiredVersionID)
}

func TestCharacterEditDoesNotChangeExistingSnapshot(t *testing.T) {
	e := newContentEnv(t)
	ctx := context.Background()
	repo := postgres.NewContentRepo(e.pool)
	resolver := content.NewResolver(repo, content.ResolverConfig{TTSMode: "fake", LLMConfigured: true}, nil)
	in := content.ResolveInput{UserID: uuid.New(), TopicID: e.ids.TopicID, ScenarioID: e.ids.ScenarioID, CharacterID: e.ids.CharacterID, ConversationStartMode: "USER_FIRST"}

	rc, err := resolver.Resolve(ctx, in)
	require.NoError(t, err)
	snap, err := rc.ConfigSnapshot.MarshalJSONB()
	require.NoError(t, err)
	user := testutil.SeedUser(t, e.pool, nil)
	sid := testutil.SeedSession(t, e.pool, user, e.ids, testutil.Row{"config_snapshot": snap})
	read := func() string {
		var s string
		require.NoError(t, e.pool.QueryRow(ctx, `SELECT config_snapshot::text FROM sessions WHERE id = $1`, sid).Scan(&s))
		return s
	}
	before := read()

	persona := strings.Repeat("Persona yang sudah diubah admin. ", 3)
	_, err = e.svc.UpdateCharacter(ctx, e.admin, e.ids.CharacterID, content.CharacterPatch{
		Persona: &persona, Voice: &content.Voice{Gender: content.GenderMale, Accent: content.AccentKansai, SpeakingRate: 0.9}})
	require.NoError(t, err)

	require.Equal(t, before, read(), "the old session keeps its snapshot byte for byte")
	rc2, err := resolver.Resolve(ctx, in)
	require.NoError(t, err)
	require.Equal(t, persona, rc2.ConfigSnapshot.Character.Persona, "a new session sees the edit")
	require.Equal(t, content.GenderMale, rc2.ConfigSnapshot.Character.Voice.Gender)
}
