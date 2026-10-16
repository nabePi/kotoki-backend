//go:build integration

package integration_test

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/content"
	"github.com/nabePi/kotoki-backend/internal/evaluation"
	"github.com/nabePi/kotoki-backend/internal/postgres"
	"github.com/nabePi/kotoki-backend/internal/prompt"
	"github.com/nabePi/kotoki-backend/internal/testutil"
)

// expectedPromptVersionCount is the number of seeded prompt versions: CONVERSATION, OPENING and
// EVALUATION. M7 raises it to 4 (SUMMARY) and M8 to 5 (MEMORY_EXTRACTION): one line each.
const expectedPromptVersionCount = 3

// The EVALUATION seed version is validated by ValidateCatalog, which needs its fixtures (spec
// 6.10.1); production registers them in cmd/*/main.go.
func init() { prompt.RegisterFixtures("EVALUATION", evaluation.PromptFixtures) }

const (
	harukaID  = "01920000-000a-7000-8000-000000000001"
	backendID = "01920000-0002-7000-8000-000000000003"
)

// runSQLFile executes one seed file (BEGIN … COMMIT) and fails on the first error, like
// psql -v ON_ERROR_STOP=1.
func runSQLFile(t *testing.T, pool *pgxpool.Pool, rel string) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", rel))
	require.NoError(t, err)
	_, err = pool.Exec(context.Background(), string(b))
	require.NoError(t, err, rel)
}

func runSeed(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	files, err := filepath.Glob(filepath.Join("..", "..", "seed", "*.sql"))
	require.NoError(t, err)
	require.NotEmpty(t, files)
	sort.Strings(files)
	for _, f := range files {
		runSQLFile(t, pool, filepath.Join("seed", filepath.Base(f)))
	}
}

func seededPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := testutil.NewPostgres(t)
	runSeed(t, pool)
	return pool
}

func counts(t *testing.T, pool *pgxpool.Pool) map[string]int {
	t.Helper()
	out := map[string]int{}
	for _, tbl := range []string{"topics", "scenarios", "scenario_versions", "characters", "prompt_configs", "prompt_versions"} {
		var n int
		require.NoError(t, pool.QueryRow(context.Background(), "SELECT count(*) FROM "+tbl).Scan(&n))
		out[tbl] = n
	}
	return out
}

func errorsAndWarnings(issues []content.Issue) (errs, warns []content.Issue) {
	for _, is := range issues {
		if is.Severity == content.SeverityError {
			errs = append(errs, is)
		} else {
			warns = append(warns, is)
		}
	}
	return errs, warns
}

func validatorFor(pool *pgxpool.Pool, ttsReal bool) content.Validator {
	return content.NewValidator(postgres.NewContentRepo(pool), content.ValidatorConfig{TTSEnabled: func(string) bool { return true }, TTSModeReal: ttsReal})
}

func TestSeedContentIsValid(t *testing.T) {
	pool := seededPool(t)
	errs, _ := errorsAndWarnings(validatorFor(pool, false).ValidateCatalog(context.Background()))
	require.Empty(t, errs)
}

func TestSeedIdempotent(t *testing.T) {
	pool := seededPool(t)
	first := counts(t, pool)
	runSeed(t, pool)
	require.Equal(t, first, counts(t, pool))
}

func TestSeedCounts(t *testing.T) {
	c := counts(t, seededPool(t))
	require.Equal(t, map[string]int{"topics": 5, "scenarios": 13, "scenario_versions": 13, "characters": 4,
		"prompt_configs": 5, "prompt_versions": expectedPromptVersionCount}, c)
}

func TestSeedScenarioFormality(t *testing.T) {
	pool := seededPool(t)
	rows, err := pool.Query(context.Background(), `SELECT s.code, sv.formality, sv.status FROM scenarios s JOIN scenario_versions sv ON sv.scenario_id = s.id`)
	require.NoError(t, err)
	defer rows.Close()
	n := 0
	for rows.Next() {
		var code, formality, status string
		require.NoError(t, rows.Scan(&code, &formality, &status))
		want := "POLITE"
		if code == "FRESH_GRADUATE_INTERVIEW" || code == "JAPANESE_COMPANY_INTERVIEW" || code == "BACKEND_ENGINEER_INTERVIEW" {
			want = "KEIGO"
		}
		require.Equal(t, want, formality, code)
		require.Equal(t, "PUBLISHED", status, code)
		n++
	}
	require.Equal(t, 13, n)
}

func TestSeedCharacters(t *testing.T) {
	pool := seededPool(t)
	chars, err := postgres.NewContentRepo(pool).ListActiveCharacters(context.Background())
	require.NoError(t, err)
	require.Len(t, chars, 4)
	by := map[string]content.Character{}
	genders := map[content.Gender]int{}
	kansai := 0
	for _, c := range chars {
		by[c.Code] = c
		genders[c.Voice.Gender]++
		if c.Voice.Accent == content.AccentKansai {
			kansai++
		}
		require.Equal(t, "PLACEHOLDER_MODEL_"+c.Code, c.ModelName)
		require.Equal(t, "PLACEHOLDER_VOICE_"+c.Code, c.VoiceID)
		require.Nil(t, c.Voice.Fallback, "no fallback until the benchmark")
		require.Contains(t, []string{"ELEVENLABS", "CARTESIA"}, c.ProviderName)
		require.NotEmpty(t, c.VoiceID)
	}
	require.Equal(t, 2, genders[content.GenderFemale])
	require.Equal(t, 2, genders[content.GenderMale])
	require.Equal(t, 1, kansai)
	require.Equal(t, content.AccentKansai, by["AOI"].Voice.Accent)
	require.Contains(t, by["AOI"].Persona, "Kansai")
	require.Contains(t, by["AOI"].Persona, "うち")

	require.InDelta(t, 1.00, by["HARUKA"].Voice.SpeakingRate, 1e-9)
	require.InDelta(t, 0.95, by["KENJI"].Voice.SpeakingRate, 1e-9)
	require.InDelta(t, 1.00, by["AOI"].Voice.SpeakingRate, 1e-9)
	require.InDelta(t, 1.00, by["RIKU"].Voice.SpeakingRate, 1e-9)
	require.Equal(t, "ELEVENLABS", by["HARUKA"].ProviderName)
	require.Equal(t, "ELEVENLABS", by["KENJI"].ProviderName)
	require.Equal(t, "CARTESIA", by["AOI"].ProviderName)
	require.Equal(t, "CARTESIA", by["RIKU"].ProviderName)
	for code, pronoun := range map[string]string{"HARUKA": "私", "KENJI": "私", "AOI": "うち", "RIKU": "僕"} {
		require.Contains(t, by[code].Persona, "「"+pronoun+"」", code)
	}

	var keys []string
	rows, err := pool.Query(context.Background(), `SELECT DISTINCT jsonb_object_keys(voice) FROM characters ORDER BY 1`)
	require.NoError(t, err)
	for rows.Next() {
		var k string
		require.NoError(t, rows.Scan(&k))
		keys = append(keys, k)
	}
	rows.Close()
	require.Equal(t, []string{"accent", "gender", "speaking_rate"}, keys, "voice holds only the non-vendor attributes")
}

func TestSeedPlaceholdersRejectedInRealMode(t *testing.T) {
	pool := seededPool(t)
	errs, _ := errorsAndWarnings(validatorFor(pool, true).ValidateCatalog(context.Background()))
	flagged := map[string]bool{}
	for _, is := range errs {
		if is.Rule == "voice_placeholder" {
			flagged[is.Entity] = true
		}
	}
	for _, code := range []string{"HARUKA", "KENJI", "AOI", "RIKU"} {
		require.True(t, flagged["characters/"+code], code)
	}
	fakeErrs, fakeWarns := errorsAndWarnings(validatorFor(pool, false).ValidateCatalog(context.Background()))
	require.Empty(t, fakeErrs)
	placeholders := 0
	for _, w := range fakeWarns {
		if w.Rule == "voice_placeholder" {
			placeholders++
		}
	}
	require.Equal(t, 4, placeholders, "fake mode only warns")
}

func TestSeedVoiceOverrideSQL(t *testing.T) {
	pool := seededPool(t)
	type voice struct{ model, id string }
	read := func() map[string]voice {
		out := map[string]voice{}
		rows, err := pool.Query(context.Background(), `SELECT code, model_name, voice_id FROM characters`)
		require.NoError(t, err)
		defer rows.Close()
		for rows.Next() {
			var code string
			var v voice
			require.NoError(t, rows.Scan(&code, &v.model, &v.id))
			out[code] = v
		}
		return out
	}
	before := read()
	runSQLFile(t, pool, "seed/env/example/0900_voice_ids.sql")
	after := read()
	require.NotEqual(t, before["HARUKA"], after["HARUKA"])
	require.Equal(t, "21m00Tcm4TlvDq8ikWAM", after["HARUKA"].id)
	for _, code := range []string{"KENJI", "AOI", "RIKU"} {
		require.Equal(t, before[code], after[code], code)
	}
	runSQLFile(t, pool, "seed/env/example/0900_voice_ids.sql")
	require.Equal(t, after, read(), "a second run changes nothing")
}

func TestSeedResolveHarukaBackend(t *testing.T) {
	pool := seededPool(t)
	resolver := content.NewResolver(postgres.NewContentRepo(pool), content.ResolverConfig{TTSMode: "fake", LLMConfigured: true}, nil)
	var topicID uuid.UUID
	require.NoError(t, pool.QueryRow(context.Background(), `SELECT id FROM topics WHERE code = 'JOB_INTERVIEW'`).Scan(&topicID))
	rc, err := resolver.Resolve(context.Background(), content.ResolveInput{UserID: uuid.New(), TopicID: topicID,
		ScenarioID: uuid.MustParse(backendID), CharacterID: uuid.MustParse(harukaID), ConversationStartMode: "USER_FIRST"})
	require.NoError(t, err)
	require.Equal(t, "ELEVENLABS", rc.ConfigSnapshot.Character.ProviderName)
	require.Equal(t, content.FormalityKeigo, rc.ConfigSnapshot.Formality)
	require.Equal(t, content.AccentKanto, rc.ConfigSnapshot.Character.Voice.Accent)
	require.Equal(t, "HARUKA", rc.ConfigSnapshot.Character.Code)
}

func publishedPrompts(t *testing.T, pool *pgxpool.Pool) map[content.PromptKey]content.PromptVersion {
	t.Helper()
	list, err := postgres.NewContentRepo(pool).ListPublishedPromptVersions(context.Background())
	require.NoError(t, err)
	out := map[content.PromptKey]content.PromptVersion{}
	for _, pv := range list {
		out[pv.Key] = pv
	}
	return out
}

func TestSeedPromptsRender(t *testing.T) {
	pool := seededPool(t)
	for _, key := range []content.PromptKey{content.PromptConversation, content.PromptOpening} {
		pv := publishedPrompts(t, pool)[key]
		small, err := prompt.Render(pv.Template(), prompt.FixtureMinimal())
		require.NoError(t, err, key)
		require.NotContains(t, small.System, "TARO-SENTINEL")
		large, err := prompt.Render(pv.Template(), prompt.FixtureMaximal())
		require.NoError(t, err, key)
		require.LessOrEqual(t, utf8.RuneCountInString(large.System), 12000, key)
		for _, banned := range []string{"<conversation_style>", "<expression", "expression_profile", "style_profile", "PLACEHOLDER_VOICE"} {
			require.NotContains(t, large.System, banned, key)
		}
	}
}

func TestSeedPromptsMatchM4Constants(t *testing.T) {
	pub := publishedPrompts(t, seededPool(t))
	conv, open := pub[content.PromptConversation], pub[content.PromptOpening]
	require.Equal(t, prompt.ConversationV1.System, conv.SystemTemplate)
	require.Equal(t, prompt.OpeningV1.System, open.SystemTemplate)
	require.Nil(t, conv.UserTemplate)
	require.Nil(t, open.UserTemplate)
	require.Empty(t, conv.OutputSchema)
	require.InDelta(t, 0.8, *conv.LLMParams.Temperature, 1e-9)
	require.Equal(t, 320, *conv.LLMParams.MaxOutputTokens)
	require.InDelta(t, 0.7, *open.LLMParams.Temperature, 1e-9)
	require.Equal(t, 120, *open.LLMParams.MaxOutputTokens)
}

func TestSeedFullNameNeverRendered(t *testing.T) {
	pool := seededPool(t)
	typ := reflect.TypeOf(prompt.PromptData{})
	var walk func(reflect.Type, string)
	walk = func(rt reflect.Type, path string) {
		if rt.Kind() != reflect.Struct {
			return
		}
		for i := 0; i < rt.NumField(); i++ {
			f := rt.Field(i)
			low := strings.ToLower(f.Name)
			require.False(t, strings.Contains(low, "fullname") || strings.Contains(low, "email"), path+"."+f.Name)
			walk(f.Type, path+"."+f.Name)
		}
	}
	walk(typ, "PromptData")

	for _, key := range []content.PromptKey{content.PromptConversation, content.PromptOpening} {
		pv := publishedPrompts(t, pool)[key]
		data := prompt.FixtureMinimal()
		data.Learner.DisplayName = ""
		out, err := prompt.Render(pv.Template(), data)
		require.NoError(t, err)
		require.NotContains(t, out.System, "FULLNAME-SENTINEL")

		data.Learner.DisplayName = "TARO-SENTINEL"
		named, err := prompt.Render(pv.Template(), data)
		require.NoError(t, err)
		require.Contains(t, named.System, "TARO-SENTINEL", "the nickname is the only name the template can use")
	}
}
