package content

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
)

// PlaceholderVoice is one voice slot still holding a PLACEHOLDER_ value (spec 6.3.5, 6.13.3).
// Slot is PRIMARY for characters.provider_name/model_name/voice_id and FALLBACK for
// characters.voice.fallback. Provider is an UPPERCASE vendor name.
type PlaceholderVoice struct {
	CharacterCode      string
	Slot               string
	Provider           string
	PlaceholderVoiceID bool
	PlaceholderModel   bool
}

// VendorCharacter is an ACTIVE character bound to a vendor whose TTS switch is off.
type VendorCharacter struct {
	CharacterCode string
	Provider      string
}

// VoiceChecker reads, for ACTIVE characters, the voice slots of the given vendors that are still
// placeholders.
type VoiceChecker interface {
	PlaceholderVoices(ctx context.Context, vendors []string) ([]PlaceholderVoice, error)
}

// VendorCharacterLister is the optional second port of a VoiceChecker: it lists ACTIVE characters
// whose primary vendor is not in enabled, so that ValidateVoices can warn about them.
type VendorCharacterLister interface {
	ActiveCharactersOutside(ctx context.Context, enabled []string) ([]VendorCharacter, error)
}

// VoiceCheckConfig tells ValidateVoices what is deployed. Vendors are the enabled TTS vendors
// (UPPERCASE; providerwire.RealDeps.TTSVendors or ConfiguredTTSVendors).
type VoiceCheckConfig struct {
	RealTTS bool
	Vendors []string
	Log     *slog.Logger // nil: no warnings are written
}

// ConfiguredTTSVendors turns the *_TTS_ENABLED switches (vendor to enabled) into the sorted list
// of enabled vendors, the same source the resolver (spec 6.9) uses.
func ConfiguredTTSVendors(enabled map[string]bool) []string {
	var out []string
	for v, on := range enabled {
		if on {
			out = append(out, strings.ToUpper(v))
		}
	}
	slices.Sort(out)
	return out
}

// ValidateVoices refuses to start a real-TTS process while an ACTIVE character of an enabled vendor
// still has a PLACEHOLDER_ voice_id or model_name, in its primary voice or its voice.fallback
// (spec 6.3.5, 6.13.3). A character bound to a vendor whose switch is off is only a WARN: the
// resolver rejects its sessions (CONTENT_MISCONFIGURED / CHARACTER_NOT_AVAILABLE).
func ValidateVoices(ctx context.Context, vc VoiceChecker, c VoiceCheckConfig) error {
	if !c.RealTTS || len(c.Vendors) == 0 {
		return nil
	}
	rows, err := vc.PlaceholderVoices(ctx, c.Vendors)
	if err != nil {
		return fmt.Errorf("content: check character voices: %w", err)
	}
	if lister, ok := vc.(VendorCharacterLister); ok && c.Log != nil {
		out, lerr := lister.ActiveCharactersOutside(ctx, c.Vendors)
		if lerr != nil {
			return fmt.Errorf("content: list characters of disabled TTS vendors: %w", lerr)
		}
		for _, ch := range out {
			c.Log.WarnContext(ctx, "active character uses a TTS vendor that is not enabled; its sessions are rejected",
				"character", ch.CharacterCode, "provider", ch.Provider, "switch", ch.Provider+"_TTS_ENABLED")
		}
	}
	if len(rows) == 0 {
		return nil
	}
	errs := make([]error, 0, len(rows))
	for _, r := range rows {
		var fields []string
		if r.PlaceholderVoiceID {
			fields = append(fields, "voice_id")
		}
		if r.PlaceholderModel {
			fields = append(fields, "model_name")
		}
		errs = append(errs, fmt.Errorf("character %s (%s, vendor %s): %s is a PLACEHOLDER_ value",
			r.CharacterCode, r.Slot, r.Provider, strings.Join(fields, " and ")))
	}
	return fmt.Errorf("content: PROVIDER_MODE real needs real voices; fill them with seed/env/<env>/0900_voice_ids.sql (spec 6.13.3): %w",
		errors.Join(errs...))
}
