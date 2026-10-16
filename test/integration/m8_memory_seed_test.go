//go:build integration

package integration_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/content"
	"github.com/nabePi/kotoki-backend/internal/memory"
	"github.com/nabePi/kotoki-backend/internal/postgres"
	"github.com/nabePi/kotoki-backend/internal/prompt"
	"github.com/nabePi/kotoki-backend/internal/provider"
	"github.com/nabePi/kotoki-backend/internal/provider/fake"
	"github.com/nabePi/kotoki-backend/internal/testutil"
)

// TestSeedPromptRowMatchesSpec checks the seeded MEMORY_EXTRACTION version (spec 11.5.2): one
// PUBLISHED version 1 with both templates, the schema and the llm_params, that renders every
// fixture, accepts the example output, and survives running the seed twice. (Plan name
// TestSeedPromptRowMatchesSpec; it lives here because internal/memory tests may not import
// internal/postgres, depguard feature-no-infra.)
func TestSeedPromptRowMatchesSpec(t *testing.T) {
	pool := testutil.NewPostgres(t)
	testutil.ApplySeed(t, pool)
	ctx := context.Background()

	pv, err := postgres.NewContentRepo(pool).GetPublishedPromptVersion(ctx, content.PromptMemoryExtraction)
	require.NoError(t, err)
	require.Equal(t, 1, pv.Version)
	require.Equal(t, content.PromptMemoryExtraction, pv.Key)
	require.Equal(t, "01920000-000d-7000-8000-000000000005", pv.ID.String())
	require.Equal(t, content.VersionPublished, pv.Status)
	require.NotNil(t, pv.UserTemplate)
	require.NotEmpty(t, pv.SystemTemplate)
	require.NotEmpty(t, pv.OutputSchema)
	require.NotNil(t, pv.LLMParams.Temperature)
	require.InDelta(t, 0, *pv.LLMParams.Temperature, 1e-9)
	require.NotNil(t, pv.LLMParams.MaxOutputTokens)
	require.Equal(t, 2000, *pv.LLMParams.MaxOutputTokens)
	require.Nil(t, pv.LLMParams.TopP)
	require.Nil(t, pv.LLMParams.Seed)
	compileSchema(t, pv.OutputSchema)

	for i, fx := range memory.PromptFixtures() {
		out, err := prompt.Render(pv.Template(), fx)
		require.NoError(t, err, "fixture %d", i)
		require.NotEmpty(t, out.User)
	}
	_, err = memory.ParseOutput(memory.ExampleOutput(), pv.OutputSchema)
	require.NoError(t, err)

	errs, _ := errorsAndWarnings(validatorFor(pool, false).ValidateCatalog(ctx))
	require.Empty(t, errs)

	before := counts(t, pool)
	testutil.ApplySeed(t, pool)
	require.Equal(t, before, counts(t, pool))
	var n int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM prompt_versions pv JOIN prompt_configs pc ON pc.id = pv.prompt_config_id WHERE pc.key = 'MEMORY_EXTRACTION'`).Scan(&n))
	require.Equal(t, 1, n)
}

// TestFakeMemoryFixtureMatchesSeedSchema: the fake evaluation provider serves the embedded
// kotoki_memory_extraction_v1 fixture for SchemaName kotoki_memory_extraction_v{version}; it parses
// against the seeded schema and validates with evidence in seq 1-3.
func TestFakeMemoryFixtureMatchesSeedSchema(t *testing.T) {
	pool := testutil.NewPostgres(t)
	testutil.ApplySeed(t, pool)
	ctx := context.Background()
	pv, err := postgres.NewContentRepo(pool).GetPublishedPromptVersion(ctx, content.PromptMemoryExtraction)
	require.NoError(t, err)

	ev := fake.NewEvaluation(provider.FakePrimary, fake.CapConfig{}, fake.Config{}, fake.Deps{Seed: 42})
	res, err := ev.GenerateJSON(ctx, provider.StructuredRequest{
		Task: "MEMORY_EXTRACTION", SchemaName: fmt.Sprintf("kotoki_memory_extraction_v%d", pv.Version), JSONSchema: pv.OutputSchema,
		RequestID: "MEMORY_EXTRACTION:" + uuid.NewString(), MaxOutputTokens: 2000,
		Messages: []provider.Message{{Role: provider.RoleSystem, Content: "s"}, {Role: provider.RoleUser, Content: "u"}},
	})
	require.NoError(t, err)
	out, err := memory.ParseOutput(res.JSON, pv.OutputSchema)
	require.NoError(t, err)

	turns := map[int]uuid.UUID{1: uuid.New(), 2: uuid.New(), 3: uuid.New(), 4: uuid.New()}
	got := memory.ValidateCandidates(out, turns, uuid.New(), nil)
	require.Len(t, got, 4)
}
