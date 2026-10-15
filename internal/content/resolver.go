package content

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	"github.com/nabePi/kotoki-backend/internal/platform/errs"
)

// ResolverConfig is injected by the composition root.
type ResolverConfig struct {
	// TTSMode is "real" or "fake" (PROVIDER_MODE_TTS ?? PROVIDER_MODE). In fake mode the
	// character's model/voice are not inspected: the fake TTS ignores them (spec 6.6.1).
	TTSMode string
	// TTSEnabled reports whether a TTS vendor is enabled by <VENDOR>_TTS_ENABLED (D-017).
	TTSEnabled func(provider string) bool
	// LLMConfigured is true when the active LLM vendor has a model configured.
	LLMConfigured bool
}

// NewResolver builds the spec 6.9 resolver. Every returned error is counted through obs.
func NewResolver(r Repository, cfg ResolverConfig, obs Observer) Resolver {
	if obs == nil {
		obs = NopObserver{}
	}
	if cfg.TTSEnabled == nil {
		cfg.TTSEnabled = func(string) bool { return true }
	}
	return &resolver{repo: r, cfg: cfg, obs: obs}
}

type resolver struct {
	repo Repository
	cfg  ResolverConfig
	obs  Observer
}

const misconfiguredMessage = "Selected configuration is temporarily unavailable"

// fail counts and returns a business error (422).
func (r *resolver) fail(code errs.Code, msg string) error {
	r.obs.ResolveError(string(code))
	return errs.New(code, msg)
}

// misconfigured logs at ERROR with the entity and rule, counts and returns CONTENT_MISCONFIGURED.
func (r *resolver) misconfigured(entity, rule string) error {
	slog.Error("content misconfigured", "entity", entity, "rule", rule)
	r.obs.ResolveError(string(errs.CodeContentMisconfigured))
	return errs.New(errs.CodeContentMisconfigured, misconfiguredMessage)
}

func internal(err error, msg string) error { return errs.Wrap(err, errs.CodeInternalError, msg) }

// Resolve follows the checks of spec 6.9 in order; the first problem found is returned.
func (r *resolver) Resolve(ctx context.Context, in ResolveInput) (ResolvedConfig, error) {
	// 1. topic and scenario
	topic, err := r.repo.GetTopic(ctx, in.TopicID)
	switch {
	case errors.Is(err, ErrNotFound):
		return ResolvedConfig{}, r.fail(errs.CodeTopicNotAvailable, "topic is not available")
	case err != nil:
		return ResolvedConfig{}, internal(err, "read topic failed")
	case topic.Status != StatusActive:
		return ResolvedConfig{}, r.fail(errs.CodeTopicNotAvailable, "topic is not available")
	}
	scn, err := r.repo.GetScenario(ctx, in.ScenarioID)
	switch {
	case errors.Is(err, ErrNotFound):
		return ResolvedConfig{}, r.fail(errs.CodeScenarioNotAvailable, "scenario is not available")
	case err != nil:
		return ResolvedConfig{}, internal(err, "read scenario failed")
	case scn.Status != StatusActive:
		return ResolvedConfig{}, r.fail(errs.CodeScenarioNotAvailable, "scenario is not available")
	case scn.TopicID != topic.ID:
		return ResolvedConfig{}, r.fail(errs.CodeScenarioTopicMismatch, "scenario does not belong to the topic")
	}
	sv, err := r.repo.GetPublishedScenarioVersion(ctx, scn.ID)
	switch {
	case errors.Is(err, ErrNotFound):
		return ResolvedConfig{}, r.fail(errs.CodeScenarioNotAvailable, "scenario is not available")
	case err != nil:
		return ResolvedConfig{}, internal(err, "read scenario version failed")
	}

	// 2. character
	ch, err := r.repo.GetCharacter(ctx, in.CharacterID)
	switch {
	case errors.Is(err, ErrNotFound):
		return ResolvedConfig{}, r.fail(errs.CodeCharacterNotAvailable, "character is not available")
	case err != nil:
		return ResolvedConfig{}, internal(err, "read character failed")
	case ch.Status != StatusActive:
		return ResolvedConfig{}, r.fail(errs.CodeCharacterNotAvailable, "character is not available")
	}

	// 3. the character's TTS
	primary := NormalizeProviderName(ch.ProviderName)
	if !r.cfg.TTSEnabled(primary) { // the vendor is switched off: the character is not offered, in either mode
		return ResolvedConfig{}, r.fail(errs.CodeCharacterNotAvailable, "character is not available")
	}
	if r.cfg.TTSMode != "fake" {
		if strings.TrimSpace(ch.ModelName) == "" || strings.TrimSpace(ch.VoiceID) == "" {
			return ResolvedConfig{}, r.misconfigured("characters/"+ch.Code, "tts_binding_empty")
		}
		if strings.HasPrefix(ch.VoiceID, placeholderPrefix) {
			return ResolvedConfig{}, r.misconfigured("characters/"+ch.Code, "voice_placeholder")
		}
		fb := ch.Voice.Fallback
		if fb == nil || !r.cfg.TTSEnabled(NormalizeProviderName(fb.ProviderName)) || strings.HasPrefix(fb.VoiceID, placeholderPrefix) {
			slog.Warn("character has no usable TTS fallback", "character", ch.Code)
			r.obs.VoiceFallbackMissing(ch.Code)
		}
	}

	// 4. prompt (OPENING is chosen later by BeginStart and may be absent, S-2)
	pr, err := r.repo.GetPublishedPromptVersion(ctx, PromptConversation)
	switch {
	case errors.Is(err, ErrNotFound):
		return ResolvedConfig{}, r.misconfigured("prompt_configs/CONVERSATION", "no_published_version")
	case err != nil:
		return ResolvedConfig{}, internal(err, "read prompt version failed")
	}

	// 5. an LLM model is configured
	if !r.cfg.LLMConfigured {
		return ResolvedConfig{}, r.misconfigured("llm", "model_not_configured")
	}

	// 6. snapshot: copy everything, so later edits of the live rows can never reach it
	return ResolvedConfig{
		TopicID: topic.ID, ScenarioID: scn.ID, ScenarioVersionID: sv.ID, CharacterID: ch.ID, PromptVersionID: pr.ID,
		ConfigSnapshot: SessionSnapshot{Character: snapshotOf(ch), Formality: sv.Formality},
	}, nil
}

func snapshotOf(ch Character) CharacterSnapshot {
	voice := ch.Voice
	if voice.Fallback != nil {
		fb := *voice.Fallback
		voice.Fallback = &fb
	}
	snap := CharacterSnapshot{
		ID: ch.ID, Code: ch.Code, Name: ch.Name, Persona: ch.Persona,
		ProviderName: NormalizeProviderName(ch.ProviderName), ModelName: ch.ModelName, VoiceID: ch.VoiceID, Voice: voice,
	}
	if ch.AvatarKey != nil {
		v := *ch.AvatarKey
		snap.AvatarKey = &v
	}
	if ch.GreetingHint != nil {
		v := *ch.GreetingHint
		snap.GreetingHint = &v
	}
	if snap.Voice.Fallback != nil {
		snap.Voice.Fallback.ProviderName = NormalizeProviderName(snap.Voice.Fallback.ProviderName)
	}
	return snap
}
