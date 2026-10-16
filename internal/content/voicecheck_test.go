package content_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/content"
)

type fakeChecker struct {
	rows    []content.PlaceholderVoice
	err     error
	got     [][]string
	skipped []content.VendorCharacter
}

func (f *fakeChecker) PlaceholderVoices(_ context.Context, vendors []string) ([]content.PlaceholderVoice, error) {
	f.got = append(f.got, vendors)
	return f.rows, f.err
}

func (f *fakeChecker) ActiveCharactersOutside(_ context.Context, _ []string) ([]content.VendorCharacter, error) {
	return f.skipped, nil
}

func TestPlaceholderVoiceRejectedAtStartup(t *testing.T) {
	ctx := context.Background()
	row := content.PlaceholderVoice{CharacterCode: "HARUKA", Slot: "PRIMARY", Provider: "ELEVENLABS", PlaceholderVoiceID: true, PlaceholderModel: true}
	fc := &fakeChecker{rows: []content.PlaceholderVoice{row}}
	err := content.ValidateVoices(ctx, fc, content.VoiceCheckConfig{RealTTS: true, Vendors: []string{"ELEVENLABS", "CARTESIA"}})
	require.ErrorContains(t, err, "HARUKA")
	require.ErrorContains(t, err, "PRIMARY")
	require.ErrorContains(t, err, "ELEVENLABS")
	require.ErrorContains(t, err, "0900_voice_ids.sql")
	require.ErrorContains(t, err, "voice_id")
	require.ErrorContains(t, err, "model_name")
	require.Equal(t, [][]string{{"ELEVENLABS", "CARTESIA"}}, fc.got)

	fc = &fakeChecker{rows: []content.PlaceholderVoice{row}}
	require.NoError(t, content.ValidateVoices(ctx, fc, content.VoiceCheckConfig{RealTTS: false, Vendors: []string{"ELEVENLABS"}}), "TTS mode fake: not checked")
	require.Empty(t, fc.got, "the checker is not even asked")

	require.NoError(t, content.ValidateVoices(ctx, &fakeChecker{}, content.VoiceCheckConfig{RealTTS: true, Vendors: []string{"ELEVENLABS"}}))
}

func TestPlaceholderVoiceErrorListsEveryRowAndOnlyTheBadField(t *testing.T) {
	fc := &fakeChecker{rows: []content.PlaceholderVoice{
		{CharacterCode: "KENJI", Slot: "PRIMARY", Provider: "ELEVENLABS", PlaceholderVoiceID: true},
		{CharacterCode: "AOI", Slot: "FALLBACK", Provider: "CARTESIA", PlaceholderModel: true},
	}}
	err := content.ValidateVoices(context.Background(), fc, content.VoiceCheckConfig{RealTTS: true, Vendors: []string{"ELEVENLABS", "CARTESIA"}})
	require.Error(t, err)
	msg := err.Error()
	require.Contains(t, msg, "KENJI")
	require.Contains(t, msg, "AOI")
	require.Contains(t, msg, "FALLBACK")
	require.Contains(t, msg, "CARTESIA")
}

func TestValidateVoicesIgnoresDisabledVendor(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	fc := &fakeChecker{skipped: []content.VendorCharacter{{CharacterCode: "AOI", Provider: "CARTESIA"}}}
	require.NoError(t, content.ValidateVoices(context.Background(), fc, content.VoiceCheckConfig{RealTTS: true, Vendors: []string{"ELEVENLABS"}, Log: log}))
	require.Equal(t, [][]string{{"ELEVENLABS"}}, fc.got, "the checker only sees the enabled vendors")
	require.Contains(t, buf.String(), "level=WARN")
	require.Contains(t, buf.String(), "AOI")
	require.Contains(t, buf.String(), "CARTESIA_TTS_ENABLED")

	fc = &fakeChecker{rows: []content.PlaceholderVoice{{CharacterCode: "HARUKA", Slot: "FALLBACK", Provider: "ELEVENLABS", PlaceholderVoiceID: true}}}
	err := content.ValidateVoices(context.Background(), fc, content.VoiceCheckConfig{RealTTS: true, Vendors: []string{"ELEVENLABS"}})
	require.ErrorContains(t, err, "FALLBACK")
}

func TestValidateVoicesPropagatesCheckerError(t *testing.T) {
	boom := errors.New("db down")
	err := content.ValidateVoices(context.Background(), &fakeChecker{err: boom}, content.VoiceCheckConfig{RealTTS: true, Vendors: []string{"ELEVENLABS"}})
	require.ErrorIs(t, err, boom)
}

func TestValidateVoicesWithoutVendorsIsANoOp(t *testing.T) {
	fc := &fakeChecker{}
	require.NoError(t, content.ValidateVoices(context.Background(), fc, content.VoiceCheckConfig{RealTTS: true}))
	require.Empty(t, fc.got)
}

func TestConfiguredTTSVendors(t *testing.T) {
	require.Equal(t, []string{"CARTESIA", "ELEVENLABS"}, content.ConfiguredTTSVendors(map[string]bool{"ELEVENLABS": true, "CARTESIA": true}))
	require.Equal(t, []string{"CARTESIA"}, content.ConfiguredTTSVendors(map[string]bool{"ELEVENLABS": false, "CARTESIA": true}))
	require.Empty(t, content.ConfiguredTTSVendors(map[string]bool{"ELEVENLABS": false}))
	require.Empty(t, content.ConfiguredTTSVendors(nil))
}

func TestConfiguredTTSVendorsUppercases(t *testing.T) {
	require.Equal(t, []string{"CARTESIA", "ELEVENLABS"}, content.ConfiguredTTSVendors(map[string]bool{"elevenlabs": true, "Cartesia": true}))
}
