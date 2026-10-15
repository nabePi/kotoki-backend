package main

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/auth"
	"github.com/nabePi/kotoki-backend/internal/content"
	"github.com/nabePi/kotoki-backend/internal/platform/errs"
)

type fakeUsers struct{ u auth.User }

func (f fakeUsers) RequireActive(context.Context, uuid.UUID) (auth.User, error) {
	if f.u.Status != auth.UserStatusActive {
		return auth.User{}, errs.New(errs.CodeAccountSuspended, "account suspended")
	}
	return f.u, nil
}

func TestUserGateAdapter(t *testing.T) {
	ctx := context.Background()
	info, err := userGate{fakeUsers{auth.User{Status: auth.UserStatusActive, TermsVersion: "2026-10-01"}}}.Gate(ctx, uuid.New())
	require.NoError(t, err)
	require.Equal(t, "ACTIVE", info.Status)
	require.Equal(t, "2026-10-01", info.TermsVersion, "the version the user accepted, for the caller to compare")

	_, err = userGate{fakeUsers{auth.User{Status: auth.UserStatusSuspended}}}.Gate(ctx, uuid.New())
	require.Equal(t, errs.CodeAccountSuspended, errs.CodeOf(err))
	_, err = userGate{fakeUsers{auth.User{Status: auth.UserStatusDeleted}}}.Gate(ctx, uuid.New())
	require.Equal(t, errs.CodeAccountSuspended, errs.CodeOf(err))
}

type fakePrompts struct {
	pv  content.PromptVersion
	err error
}

func (f fakePrompts) GetPublishedPromptVersion(context.Context, content.PromptKey) (content.PromptVersion, error) {
	return f.pv, f.err
}

func TestOpeningPromptSourceAdapter(t *testing.T) {
	ctx := context.Background()
	id := uuid.New()
	got, err := openingPromptSource{fakePrompts{pv: content.PromptVersion{ID: id}}}.PublishedPromptVersionID(ctx, "OPENING")
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, id, *got)

	got, err = openingPromptSource{fakePrompts{err: content.ErrNotFound}}.PublishedPromptVersionID(ctx, "OPENING")
	require.NoError(t, err, "no PUBLISHED version is not an error")
	require.Nil(t, got)

	_, err = openingPromptSource{fakePrompts{err: context.DeadlineExceeded}}.PublishedPromptVersionID(ctx, "OPENING")
	require.ErrorIs(t, err, context.DeadlineExceeded)
}

func TestResolverAndValidatorConfigFromProviderSettings(t *testing.T) {
	t.Setenv("PROVIDER_MODE", "real")
	t.Setenv("PROVIDER_MODE_TTS", "")
	t.Setenv("CARTESIA_TTS_ENABLED", "false")
	rc, err := loadProviderSettings()
	require.NoError(t, err)

	r := resolverConfig(rc)
	require.Equal(t, "real", r.TTSMode)
	require.True(t, r.TTSEnabled("ELEVENLABS"))
	require.False(t, r.TTSEnabled("CARTESIA"), "switched off by CARTESIA_TTS_ENABLED=false")
	require.True(t, r.LLMConfigured, "the default OpenAI LLM model counts as configured")
	v := validatorConfig(rc)
	require.True(t, v.TTSModeReal)
	require.False(t, v.TTSEnabled("CARTESIA"))

	t.Setenv("PROVIDER_MODE", "fake")
	rc, err = loadProviderSettings()
	require.NoError(t, err)
	require.Equal(t, "fake", resolverConfig(rc).TTSMode)
	require.False(t, validatorConfig(rc).TTSModeReal)
	require.True(t, resolverConfig(rc).LLMConfigured)

	t.Setenv("PROVIDER_MODE", "real")
	t.Setenv("OPENAI_LLM_ENABLED", "false")
	t.Setenv("DEEPSEEK_LLM_ENABLED", "false")
	rc, err = loadProviderSettings()
	require.NoError(t, err)
	require.False(t, resolverConfig(rc).LLMConfigured, "real mode without an active LLM vendor")
}
