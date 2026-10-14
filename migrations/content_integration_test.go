//go:build integration

package migrations_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/testutil"
)

type family struct {
	table, parentTable, parentCol, contentCol, statusTS, published string
	mkParent                                                       func(t *testing.T, db testutil.DBTX) uuid.UUID
	mkVersion                                                      func(parent uuid.UUID, version int, o testutil.Row) testutil.Row
}

var (
	promptKeys = []string{"CONVERSATION", "OPENING", "SUMMARY", "EVALUATION", "MEMORY_EXTRACTION"}
	promptKeyN int
)

func families() []family {
	ctx := context.Background()
	return []family{
		{
			table: "scenario_versions", parentTable: "scenarios", parentCol: "scenario_id", contentCol: "situation",
			statusTS: "chk_scenario_versions_status_ts", published: "ux_scenario_versions_published",
			mkParent: func(t *testing.T, db testutil.DBTX) uuid.UUID {
				tp := testutil.TopicRow(nil)
				testutil.MustInsert(t, ctx, db, "topics", tp)
				sc := testutil.ScenarioRow(tp["id"].(uuid.UUID), nil)
				testutil.MustInsert(t, ctx, db, "scenarios", sc)
				return sc["id"].(uuid.UUID)
			},
			mkVersion: testutil.ScenarioVersionRow,
		},
		{
			table: "prompt_versions", parentTable: "prompt_configs", parentCol: "prompt_config_id", contentCol: "system_template",
			statusTS: "chk_pv_status_ts", published: "ux_prompt_versions_published",
			mkParent: func(t *testing.T, db testutil.DBTX) uuid.UUID {
				promptKeyN++
				pc := testutil.PromptConfigRow(promptKeys[promptKeyN%len(promptKeys)], nil)
				testutil.MustInsert(t, ctx, db, "prompt_configs", pc)
				return pc["id"].(uuid.UUID)
			},
			mkVersion: testutil.PromptVersionRow,
		},
	}
}

func (f family) pub(t *testing.T, db testutil.DBTX, parent uuid.UUID, v int) uuid.UUID {
	r := f.mkVersion(parent, v, testutil.Row{"status": "PUBLISHED", "published_at": time.Now().UTC()})
	testutil.MustInsert(t, context.Background(), db, f.table, r)
	return r["id"].(uuid.UUID)
}

func (f family) draft(t *testing.T, db testutil.DBTX, parent uuid.UUID, v int) uuid.UUID {
	r := f.mkVersion(parent, v, nil)
	testutil.MustInsert(t, context.Background(), db, f.table, r)
	return r["id"].(uuid.UUID)
}

func exec(t *testing.T, db testutil.DBTX, sql string, args ...any) error {
	t.Helper()
	_, err := db.Exec(context.Background(), sql, args...)
	return err
}

func TestVersionFamilies(t *testing.T) {
	pool := testutil.NewPostgres(t)
	for _, f := range families() {
		t.Run(f.table, func(t *testing.T) {
			ctx := context.Background()
			t.Run("TestVersionOnePublished", func(t *testing.T) {
				p := f.mkParent(t, pool)
				f.pub(t, pool, p, 1)
				r := f.mkVersion(p, 2, testutil.Row{"status": "PUBLISHED", "published_at": time.Now().UTC()})
				testutil.RequirePgErr(t, testutil.TryInsert(ctx, pool, f.table, r), "23505", f.published)
			})
			t.Run("TestVersionImmutableContent", func(t *testing.T) {
				p := f.mkParent(t, pool)
				id := f.pub(t, pool, p, 1)
				err := exec(t, pool, "UPDATE "+f.table+" SET "+f.contentCol+"='changed' WHERE id=$1", id)
				testutil.RequirePgErr(t, err, "23514", "")
				require.Contains(t, err.Error(), "immutable")
			})
			t.Run("TestVersionTransitions", func(t *testing.T) {
				p := f.mkParent(t, pool)
				d := f.draft(t, pool, p, 1)
				err := exec(t, pool, "UPDATE "+f.table+" SET status='RETIRED', published_at=now(), retired_at=now() WHERE id=$1", d)
				testutil.RequirePgErr(t, err, "23514", "")
				require.Contains(t, err.Error(), "invalid content version transition")

				require.NoError(t, exec(t, pool, "UPDATE "+f.table+" SET status='PUBLISHED', published_at=now() WHERE id=$1", d))
				require.NoError(t, exec(t, pool, "UPDATE "+f.table+" SET status='RETIRED', retired_at=now() WHERE id=$1", d))
				// rollback RETIRED -> PUBLISHED needs no other PUBLISHED sibling
				require.NoError(t, exec(t, pool, "UPDATE "+f.table+" SET status='PUBLISHED', retired_at=NULL WHERE id=$1", d))

				testutil.RequirePgErr(t, exec(t, pool, "DELETE FROM "+f.table+" WHERE id=$1", d), "23514", "")
				d2 := f.draft(t, pool, p, 2)
				require.NoError(t, exec(t, pool, "DELETE FROM "+f.table+" WHERE id=$1", d2))
			})
			t.Run("TestStatusTimestamps", func(t *testing.T) {
				p := f.mkParent(t, pool)
				r := f.mkVersion(p, 1, testutil.Row{"status": "PUBLISHED"})
				testutil.RequirePgErr(t, testutil.TryInsert(ctx, pool, f.table, r), "23514", f.statusTS)
			})
		})
	}
}

func TestGuardNotOnCharacters(t *testing.T) {
	pool := testutil.NewPostgres(t)
	ctx := context.Background()
	var n int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM pg_trigger WHERE tgrelid='characters'::regclass AND NOT tgisinternal`).Scan(&n))
	require.Zero(t, n)
	ids := testutil.SeedContent(t, pool)
	require.NoError(t, exec(t, pool, "UPDATE characters SET persona=$2 WHERE id=$1", ids.CharacterID, strings.Repeat("Baru ", 20)))
	require.NoError(t, exec(t, pool, "UPDATE characters SET name='Baru' WHERE id=$1", ids.CharacterID))
	require.NoError(t, exec(t, pool, "UPDATE characters SET voice=$2 WHERE id=$1", ids.CharacterID, testutil.ValidVoiceJSON()))
	require.NoError(t, exec(t, pool, "DELETE FROM characters WHERE id=$1", ids.CharacterID))
}

func TestContentChecks(t *testing.T) {
	pool := testutil.NewPostgres(t)
	ctx := context.Background()
	tp := testutil.TopicRow(nil)
	testutil.MustInsert(t, ctx, pool, "topics", tp)
	sc := testutil.ScenarioRow(tp["id"].(uuid.UUID), nil)
	testutil.MustInsert(t, ctx, pool, "scenarios", sc)
	sid := sc["id"].(uuid.UUID)

	ins := func(table string, r testutil.Row) error { return testutil.TryInsert(ctx, pool, table, r) }
	cases := []struct {
		name       string
		err        func() error
		state, con string
	}{
		{"topic code", func() error { return ins("topics", testutil.TopicRow(testutil.Row{"code": "lower"})) }, "23514", "chk_topics_code"},
		{"difficulty", func() error { return ins("scenario_versions", testutil.ScenarioVersionRow(sid, 1, testutil.Row{"difficulty_level": 6})) }, "23514", "chk_scenario_versions_difficulty"},
		{"levels", func() error {
			return ins("scenario_versions", testutil.ScenarioVersionRow(sid, 1, testutil.Row{"recommended_level_min": 4.0, "recommended_level_max": 2.0}))
		}, "23514", "chk_scenario_versions_levels"},
		{"formality", func() error { return ins("scenario_versions", testutil.ScenarioVersionRow(sid, 1, testutil.Row{"formality": "LOUD"})) }, "23514", "chk_scenario_versions_formality"},
		{"prompt key", func() error { return ins("prompt_configs", testutil.PromptConfigRow("X", nil)) }, "23514", "chk_prompt_configs_key"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { testutil.RequirePgErr(t, c.err(), c.state, c.con) })
	}

	t.Run("formality default POLITE", func(t *testing.T) {
		r := testutil.ScenarioVersionRow(sid, 9, nil)
		testutil.MustInsert(t, ctx, pool, "scenario_versions", r)
		var f string
		require.NoError(t, pool.QueryRow(ctx, "SELECT formality FROM scenario_versions WHERE id=$1", r["id"]).Scan(&f))
		require.Equal(t, "POLITE", f)
	})
}

func TestCharacterChecks(t *testing.T) {
	pool := testutil.NewPostgres(t)
	ctx := context.Background()
	ch := func(o testutil.Row) error { return testutil.TryInsert(ctx, pool, "characters", testutil.CharacterRow(o)) }
	voice := func(o map[string]any) testutil.Row { return testutil.Row{"voice": testutil.ValidVoiceJSON(o)} }
	bad := func(name string, err func() error, state, con string) {
		t.Run(name, func(t *testing.T) { testutil.RequirePgErr(t, err(), state, con) })
	}
	good := func(name string, err func() error) {
		t.Run(name, func(t *testing.T) { require.NoError(t, err()) })
	}
	bad("code", func() error { return ch(testutil.Row{"code": "haruka"}) }, "23514", "chk_characters_code")
	bad("duplicate code", func() error {
		_ = ch(testutil.Row{"code": "DUP_CODE"})
		return ch(testutil.Row{"code": "DUP_CODE"})
	}, "23505", "ux_characters_code")
	bad("status", func() error { return ch(testutil.Row{"status": "GONE"}) }, "23514", "chk_characters_status")
	bad("persona short", func() error { return ch(testutil.Row{"persona": "0123456789"}) }, "23514", "chk_characters_persona")
	bad("name long", func() error { return ch(testutil.Row{"name": strings.Repeat("a", 51)}) }, "23514", "chk_characters_name")
	// PostgreSQL evaluates CHECKs alphabetically, so voice_accent fires before voice_obj for a non-object.
	bad("voice array", func() error { return ch(testutil.Row{"voice": []byte(`[]`)}) }, "23514", "")
	bad("voice gender", func() error { return ch(voice(map[string]any{"gender": "X"})) }, "23514", "chk_characters_voice_gender")
	bad("voice accent", func() error { return ch(voice(map[string]any{"accent": "OSAKA"})) }, "23514", "chk_characters_voice_accent")
	bad("rate low", func() error { return ch(voice(map[string]any{"speaking_rate": 0.79})) }, "23514", "chk_characters_voice_rate")
	bad("rate high", func() error { return ch(voice(map[string]any{"speaking_rate": 1.21})) }, "23514", "chk_characters_voice_rate")
	bad("rate string", func() error { return ch(voice(map[string]any{"speaking_rate": "1.0"})) }, "23514", "chk_characters_voice_rate")
	good("rate 0.80", func() error { return ch(voice(map[string]any{"speaking_rate": 0.80})) })
	good("rate 1.20", func() error { return ch(voice(map[string]any{"speaking_rate": 1.20})) })
	for _, p := range []string{"GOOGLE", "DEEPGRAM", "OPENAI", "elevenlabs"} {
		bad("provider "+p, func() error { return ch(testutil.Row{"provider_name": p}) }, "23514", "chk_characters_provider")
	}
	good("provider CARTESIA", func() error { return ch(testutil.Row{"provider_name": "CARTESIA"}) })
	for _, m := range []string{"", "   ", strings.Repeat("m", 201)} {
		bad("model "+m[:min(len(m), 3)], func() error { return ch(testutil.Row{"model_name": m}) }, "23514", "chk_characters_model")
	}
	good("model with spaces", func() error { return ch(testutil.Row{"model_name": "Eleven v3 Conversational"}) })
	bad("voice_id empty", func() error { return ch(testutil.Row{"voice_id": ""}) }, "23514", "chk_characters_voice_id")
	bad("voice_id long", func() error { return ch(testutil.Row{"voice_id": strings.Repeat("v", 201)}) }, "23514", "chk_characters_voice_id")
	good("voice_id uuid", func() error { return ch(testutil.Row{"voice_id": uuid.NewString()}) })
	good("voice_id elevenlabs", func() error { return ch(testutil.Row{"voice_id": "21m00Tcm4TlvDq8ikWAM"}) })
	for _, col := range []string{"provider_name", "model_name", "voice_id", "voice"} {
		bad("null "+col, func() error {
			r := testutil.CharacterRow(nil)
			delete(r, col)
			return testutil.TryInsert(ctx, pool, "characters", r)
		}, "23502", "")
	}
	bad("fallback not object", func() error { return ch(voice(map[string]any{"fallback": "x"})) }, "23514", "chk_characters_voice_fallback")
	good("fallback object", func() error { return ch(voice(map[string]any{"fallback": map[string]any{"provider": "CARTESIA"}})) })
	good("foreign key providers accepted by DB", func() error { return ch(voice(map[string]any{"providers": map[string]any{}})) })
}

func TestCharactersTTSColumns(t *testing.T) {
	pool := testutil.NewPostgres(t)
	for _, col := range []string{"provider_name", "model_name", "voice_id"} {
		var typ, nullable string
		require.NoError(t, pool.QueryRow(context.Background(),
			`SELECT data_type, is_nullable FROM information_schema.columns WHERE table_name='characters' AND column_name=$1`, col).Scan(&typ, &nullable))
		require.Equal(t, "text", typ, col)
		require.Equal(t, "NO", nullable, col)
	}
}

func regclassNull(t *testing.T, pool *pgxpool.Pool, name string) bool {
	var isNull bool
	require.NoError(t, pool.QueryRow(context.Background(), `SELECT to_regclass($1) IS NULL`, name).Scan(&isNull))
	return isNull
}

func TestNoLegacyContentTables(t *testing.T) {
	pool := testutil.NewPostgres(t)
	for _, n := range []string{"conversation_styles", "conversation_style_versions", "expression_profiles",
		"expression_profile_versions", "character_versions", "voice_profiles", "voice_provider_configs", "character_voice_profiles"} {
		require.True(t, regclassNull(t, pool, n), n)
	}
}

func TestSeedContent(t *testing.T) {
	pool := testutil.NewPostgres(t)
	ids := testutil.SeedContent(t, pool)
	require.NotEqual(t, uuid.Nil, ids.TopicID)
	require.NotEqual(t, uuid.Nil, ids.ScenarioVersionID)
	require.NotEqual(t, uuid.Nil, ids.CharacterID)
	require.NotEqual(t, uuid.Nil, ids.PromptVersionID)
}
