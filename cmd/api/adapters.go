package main

import (
	"context"
	"errors"
	"log/slog"
	"slices"

	"github.com/caarlos0/env/v11"
	"github.com/google/uuid"

	"github.com/nabePi/kotoki-backend/internal/auth"
	"github.com/nabePi/kotoki-backend/internal/content"
	"github.com/nabePi/kotoki-backend/internal/provider"
	"github.com/nabePi/kotoki-backend/internal/session"
)

// userReader is the part of *auth.UserService the session gate needs.
type userReader interface {
	RequireActive(ctx context.Context, userID uuid.UUID) (auth.User, error)
}

// userGate adapts auth.UserService to session.UserGate (spec 4.6.1 step 1): an account that is not
// ACTIVE is ACCOUNT_SUSPENDED as auth reports it; the terms version the user accepted is returned so
// that session.Service can compare it with the current one.
type userGate struct{ users userReader }

var _ session.UserGate = userGate{}

func (g userGate) Gate(ctx context.Context, userID uuid.UUID) (session.UserGateInfo, error) {
	u, err := g.users.RequireActive(ctx, userID)
	if err != nil {
		return session.UserGateInfo{}, err
	}
	return session.UserGateInfo{Status: string(u.Status), TermsVersion: u.TermsVersion}, nil
}

// publishedPrompts is the part of content.Repository the opening-prompt adapter reads.
type publishedPrompts interface {
	GetPublishedPromptVersion(ctx context.Context, key content.PromptKey) (content.PromptVersion, error)
}

// openingPromptSource adapts the content repository to session.OpeningPromptSource: the PUBLISHED
// version id of a prompt key, or nil when there is none (S-2).
type openingPromptSource struct{ repo publishedPrompts }

var _ session.OpeningPromptSource = openingPromptSource{}

func (s openingPromptSource) PublishedPromptVersionID(ctx context.Context, key string) (*uuid.UUID, error) {
	pv, err := s.repo.GetPublishedPromptVersion(ctx, content.PromptKey(key))
	if errors.Is(err, content.ErrNotFound) {
		return nil, nil //nolint:nilnil // "no PUBLISHED version" is a normal answer, not an error
	}
	if err != nil {
		return nil, err
	}
	id := pv.ID
	return &id, nil
}

// providerSettings is what content needs to know about the provider layer: the TTS mode, the enabled
// TTS vendors and whether an LLM is configured. It reads the same PROVIDER_* variables as the router
// (spec 8.5.1) without validating keys, which only the realtime and worker processes need.
func loadProviderSettings() (provider.RouterConfig, error) {
	var rc provider.RouterConfig
	if err := env.Parse(&rc); err != nil {
		return provider.RouterConfig{}, err
	}
	return rc, nil
}

func ttsEnabled(rc provider.RouterConfig) func(string) bool {
	vendors := rc.TTSVendors()
	return func(name string) bool { return slices.Contains(vendors, provider.Name(name)) }
}

// resolverConfig is content.ResolverConfig from the provider settings: fake TTS does not inspect the
// character's voice; an LLM counts as configured when the mode is fake or the active vendor has a model.
func resolverConfig(rc provider.RouterConfig) content.ResolverConfig {
	return content.ResolverConfig{
		TTSMode:       rc.ModeFor(provider.CapTTS),
		TTSEnabled:    ttsEnabled(rc),
		LLMConfigured: rc.ModeFor(provider.CapLLM) == provider.ModeFake || rc.LLMModel() != "",
	}
}

// validatorConfig is content.ValidatorConfig from the provider settings.
func validatorConfig(rc provider.RouterConfig) content.ValidatorConfig {
	return content.ValidatorConfig{TTSEnabled: ttsEnabled(rc), TTSModeReal: rc.ModeFor(provider.CapTTS) == provider.ModeReal}
}

// voiceCheckConfig is the input of content.ValidateVoices from the provider settings: the enabled TTS
// vendors are those of the *_TTS_ENABLED switches, the same source as the resolver (spec 6.9).
func voiceCheckConfig(rc provider.RouterConfig, logger *slog.Logger) content.VoiceCheckConfig {
	return content.VoiceCheckConfig{
		RealTTS: rc.ModeFor(provider.CapTTS) == provider.ModeReal,
		Vendors: content.ConfiguredTTSVendors(map[string]bool{
			string(provider.ElevenLabs): rc.ElevenLabsTTSEnabled, string(provider.Cartesia): rc.CartesiaTTSEnabled,
		}),
		Log: logger,
	}
}
