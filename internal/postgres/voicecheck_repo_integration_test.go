//go:build integration

package postgres_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/content"
	"github.com/nabePi/kotoki-backend/internal/postgres"
	"github.com/nabePi/kotoki-backend/internal/testutil"
)

func TestVoiceCheckRepoFindsPlaceholders(t *testing.T) {
	ctx := context.Background()
	pool := testutil.NewPostgres(t)
	testutil.ApplySeed(t, pool)
	repo := postgres.NewVoiceCheckRepo(pool)
	var _ content.VoiceChecker = repo
	var _ content.VendorCharacterLister = repo
	both := []string{"ELEVENLABS", "CARTESIA"}

	rows, err := repo.PlaceholderVoices(ctx, both)
	require.NoError(t, err)
	got := map[string]content.PlaceholderVoice{}
	for _, r := range rows {
		require.Equal(t, "PRIMARY", r.Slot)
		require.True(t, r.PlaceholderVoiceID)
		require.True(t, r.PlaceholderModel)
		got[r.CharacterCode] = r
	}
	require.Len(t, rows, 4)
	require.Equal(t, "ELEVENLABS", got["HARUKA"].Provider)
	require.Equal(t, "ELEVENLABS", got["KENJI"].Provider)
	require.Equal(t, "CARTESIA", got["AOI"].Provider)
	require.Equal(t, "CARTESIA", got["RIKU"].Provider)

	rows, err = repo.PlaceholderVoices(ctx, []string{"ELEVENLABS"})
	require.NoError(t, err)
	require.Len(t, rows, 2)
	rows, err = repo.PlaceholderVoices(ctx, nil)
	require.NoError(t, err)
	require.Empty(t, rows)

	// a vendor whose switch is off is listed for the WARN, not as a placeholder
	out, err := repo.ActiveCharactersOutside(ctx, []string{"ELEVENLABS"})
	require.NoError(t, err)
	require.Equal(t, []content.VendorCharacter{{CharacterCode: "AOI", Provider: "CARTESIA"}, {CharacterCode: "RIKU", Provider: "CARTESIA"}}, out)

	// only voice_id filled: the row stays, with the model flag alone
	_, err = pool.Exec(ctx, `UPDATE characters SET voice_id = 'real-1'`)
	require.NoError(t, err)
	rows, err = repo.PlaceholderVoices(ctx, both)
	require.NoError(t, err)
	require.Len(t, rows, 4)
	for _, r := range rows {
		require.False(t, r.PlaceholderVoiceID)
		require.True(t, r.PlaceholderModel)
	}

	// DISABLED characters are not counted
	_, err = pool.Exec(ctx, `UPDATE characters SET status = 'DISABLED' WHERE code = 'HARUKA'`)
	require.NoError(t, err)
	rows, err = repo.PlaceholderVoices(ctx, both)
	require.NoError(t, err)
	require.Len(t, rows, 3)
	_, err = pool.Exec(ctx, `UPDATE characters SET status = 'ACTIVE' WHERE code = 'HARUKA'`)
	require.NoError(t, err)

	// everything filled: nothing left
	_, err = pool.Exec(ctx, `UPDATE characters SET voice_id = 'real-1', model_name = 'real-model'`)
	require.NoError(t, err)
	rows, err = repo.PlaceholderVoices(ctx, both)
	require.NoError(t, err)
	require.Empty(t, rows)

	// a placeholder fallback of another vendor is its own FALLBACK row
	_, err = pool.Exec(ctx, `UPDATE characters SET voice = jsonb_set(voice, '{fallback}',
		'{"provider_name":"CARTESIA","model_name":"PLACEHOLDER_M","voice_id":"PLACEHOLDER_V"}')
		WHERE provider_name = 'ELEVENLABS' AND code = 'HARUKA'`)
	require.NoError(t, err)
	rows, err = repo.PlaceholderVoices(ctx, both)
	require.NoError(t, err)
	require.Equal(t, []content.PlaceholderVoice{{CharacterCode: "HARUKA", Slot: "FALLBACK", Provider: "CARTESIA", PlaceholderVoiceID: true, PlaceholderModel: true}}, rows)
	rows, err = repo.PlaceholderVoices(ctx, []string{"ELEVENLABS"})
	require.NoError(t, err)
	require.Empty(t, rows, "the fallback vendor is not enabled")

	// the schema only knows the two TTS vendors
	_, err = pool.Exec(ctx, `UPDATE characters SET provider_name = 'DEEPGRAM' WHERE code = 'KENJI'`)
	require.Error(t, err)
	require.Equal(t, "23514", pgCode(err))
}
