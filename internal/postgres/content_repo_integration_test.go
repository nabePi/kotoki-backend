//go:build integration

package postgres_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/content"
	"github.com/nabePi/kotoki-backend/internal/postgres"
	"github.com/nabePi/kotoki-backend/internal/testutil"
)

type pgContent struct {
	*pgBalance
	ids  testutil.ContentIDs
	repo content.Repository
}

func newPGContent(t *testing.T) *pgContent {
	t.Helper()
	pool := testutil.NewPostgres(t)
	store := postgres.NewStore(pool)
	return &pgContent{pgBalance: &pgBalance{pool: pool, store: store}, ids: testutil.SeedContent(t, pool), repo: postgres.NewContentRepo(pool)}
}

func pgCode(err error) string {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.Code
	}
	return ""
}

func TestContentRepoGetPublishedReturnsOnlyPublished(t *testing.T) {
	c := newPGContent(t)
	ctx := context.Background()
	draft := testutil.ScenarioVersionRow(c.ids.ScenarioID, 2, nil)
	testutil.MustInsert(ctx, t, c.pool, "scenario_versions", draft)

	got, err := c.repo.GetPublishedScenarioVersion(ctx, c.ids.ScenarioID)
	require.NoError(t, err)
	require.Equal(t, c.ids.ScenarioVersionID, got.ID)
	require.Equal(t, content.VersionPublished, got.Status)

	pv, err := c.repo.GetPublishedPromptVersion(ctx, content.PromptKey("CONVERSATION"))
	require.NoError(t, err)
	require.Equal(t, c.ids.PromptVersionID, pv.ID)

	_, err = c.repo.GetPublishedScenarioVersion(ctx, uuid.New())
	require.ErrorIs(t, err, content.ErrNotFound)
}

func TestContentRepoLoadByIDIgnoresStatus(t *testing.T) {
	c := newPGContent(t)
	ctx := context.Background()
	draft := testutil.ScenarioVersionRow(c.ids.ScenarioID, 2, nil)
	testutil.MustInsert(ctx, t, c.pool, "scenario_versions", draft)
	got, err := c.repo.GetScenarioVersion(ctx, draft["id"].(uuid.UUID))
	require.NoError(t, err)
	require.Equal(t, content.VersionDraft, got.Status)
	require.Equal(t, 2, got.Version)
	require.InDelta(t, 3.0, got.RecommendedLevelMax, 0.01)
}

func TestContentRepoPublishTwiceSameParentUniqueViolation(t *testing.T) {
	c := newPGContent(t)
	ctx := context.Background()
	row := testutil.ScenarioVersionRow(c.ids.ScenarioID, 2, testutil.Row{"status": "PUBLISHED", "published_at": time.Now().UTC()})
	err := testutil.TryInsert(ctx, c.pool, "scenario_versions", row)
	require.Equal(t, "23505", pgCode(err), "%v", err)
}

func TestContentRepoVersionImmutable(t *testing.T) {
	c := newPGContent(t)
	ctx := context.Background()
	_, err := c.pool.Exec(ctx, `UPDATE scenario_versions SET situation = 'lain' WHERE id = $1`, c.ids.ScenarioVersionID)
	require.Error(t, err)
	_, err = c.pool.Exec(ctx, `DELETE FROM scenario_versions WHERE id = $1`, c.ids.ScenarioVersionID)
	require.Error(t, err)
}

func TestContentRepoNextVersionNumberUnderParentLock(t *testing.T) {
	c := newPGContent(t)
	ctx := context.Background()
	var mu sync.Mutex
	var got []int
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := c.store.WithinTx(ctx, func(ctx context.Context, tx *postgres.Tx) error {
				r := tx.Content()
				parent, err := r.LockParent(ctx, content.FamilyScenarios, c.ids.ScenarioID.String())
				if err != nil {
					return err
				}
				n, err := r.NextVersionNumber(ctx, content.FamilyScenarios, parent)
				if err != nil {
					return err
				}
				row := testutil.ScenarioVersionRow(parent, n, nil)
				if err := r.InsertScenarioVersion(ctx, content.ScenarioVersion{ID: row["id"].(uuid.UUID), ScenarioID: parent, Version: n,
					DifficultyLevel: 2, RecommendedLevelMin: 1, RecommendedLevelMax: 3, Situation: "s", AIRole: "a", UserRole: "u", Goal: "g",
					Formality: content.FormalityPolite, OpeningLine: "o", CreatedAt: time.Now().UTC()}); err != nil {
					return err
				}
				mu.Lock()
				got = append(got, n)
				mu.Unlock()
				return nil
			})
			require.NoError(t, err)
		}()
	}
	wg.Wait()
	require.ElementsMatch(t, []int{2, 3}, got)
}

func TestContentRepoCharacterRoundTrip(t *testing.T) {
	c := newPGContent(t)
	ctx := context.Background()
	voice := testutil.ValidVoiceJSON(map[string]any{"fallback": map[string]any{"provider_name": "GOOGLE", "model_name": "PLACEHOLDER_M", "voice_id": "PLACEHOLDER_V"}})
	row := testutil.CharacterRow(testutil.Row{"voice": voice})
	testutil.MustInsert(ctx, t, c.pool, "characters", row)
	got, err := c.repo.GetCharacter(ctx, row["id"].(uuid.UUID))
	require.NoError(t, err)
	require.Equal(t, "ELEVENLABS", got.ProviderName)
	require.Equal(t, content.GenderFemale, got.Voice.Gender)
	require.NotNil(t, got.Voice.Fallback)
	require.Equal(t, "PLACEHOLDER_V", got.Voice.Fallback.VoiceID)
}

func TestContentRepoUpdateCharacterLockAndMerge(t *testing.T) {
	c := newPGContent(t)
	ctx := context.Background()
	var wg sync.WaitGroup
	patch := func(f func(*content.Character)) {
		defer wg.Done()
		require.NoError(t, c.store.WithinTx(ctx, func(ctx context.Context, tx *postgres.Tx) error {
			r := tx.Content()
			ch, err := r.LockCharacter(ctx, c.ids.CharacterID)
			if err != nil {
				return err
			}
			f(&ch)
			// The timestamp comes from the database clock: created_at was set by now() in the container,
			// and the container's clock may run ahead of the host's (time.Now() made this test flaky).
			var at time.Time
			if err := tx.DBTX().QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&at); err != nil {
				return err
			}
			return r.UpdateCharacter(ctx, ch, at.UTC())
		}))
	}
	wg.Add(2)
	go patch(func(ch *content.Character) { ch.Name = "Baru" })
	go patch(func(ch *content.Character) {
		ch.Persona = "Persona baru yang cukup panjang untuk lolos batas minimal lima puluh karakter."
	})
	wg.Wait()
	got, err := c.repo.GetCharacter(ctx, c.ids.CharacterID)
	require.NoError(t, err)
	require.Equal(t, "Baru", got.Name)
	require.Equal(t, "Persona baru yang cukup panjang untuk lolos batas minimal lima puluh karakter.", got.Persona)
	require.True(t, got.UpdatedAt.After(got.CreatedAt) || got.UpdatedAt.Equal(got.CreatedAt))
}

func TestContentRepoGetUserVoiceExposure(t *testing.T) {
	c := newPGContent(t)
	ctx := context.Background()
	user := testutil.SeedUser(t, c.pool, nil)
	mk := func(status, gender, accent string, charged int) {
		row := testutil.SessionRow(user, c.ids, testutil.Row{"status": status})
		snap := map[string]any{"character": map[string]any{"voice": map[string]any{"gender": gender, "accent": accent, "speaking_rate": 1.0}}}
		b, _ := json.Marshal(snap)
		row["config_snapshot"] = b
		for k, v := range completedCols(status, charged) {
			row[k] = v
		}
		testutil.MustInsert(ctx, t, c.pool, "sessions", row)
	}
	mk("COMPLETED", "FEMALE", "KANTO", 60)
	mk("COMPLETED", "FEMALE", "KANTO", 30)
	mk("COMPLETED", "MALE", "KANSAI", 45)
	mk("CANCELLED", "MALE", "KANSAI", 0)

	_, err := c.pool.Exec(ctx, `UPDATE characters SET voice = jsonb_set(voice, '{gender}', '"MALE"') WHERE id = $1`, c.ids.CharacterID)
	require.NoError(t, err)

	rows, err := c.store.Queries().GetUserVoiceExposure(ctx, user)
	require.NoError(t, err)
	by := map[string][2]int64{}
	for _, r := range rows {
		by[r.Gender+"_"+r.Accent] = [2]int64{int64(r.SessionCount), r.Seconds}
	}
	require.Equal(t, [2]int64{2, 90}, by["FEMALE_KANTO"])
	require.Equal(t, [2]int64{1, 45}, by["MALE_KANSAI"])
}

// completedCols returns the columns chk_sessions_state demands for a terminal session.
func completedCols(status string, charged int) testutil.Row {
	started := time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)
	ended := started.Add(time.Duration(charged) * time.Second)
	switch status {
	case "COMPLETED":
		return testutil.Row{"end_reason": "USER_ENDED", "starting_at": started, "started_at": started,
			"expires_at": started.Add(300 * time.Second), "ending_at": ended, "ended_at": ended, "charged_seconds": charged}
	default: // CANCELLED
		return testutil.Row{"end_reason": "USER_CANCELLED", "ended_at": ended, "charged_seconds": 0}
	}
}
