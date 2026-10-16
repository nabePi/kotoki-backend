package postgres

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/nabePi/kotoki-backend/internal/content"
	"github.com/nabePi/kotoki-backend/internal/platform/errs"
	"github.com/nabePi/kotoki-backend/internal/postgres/sqlcgen"
)

// VoiceCheckRepo answers the startup voice check of PROVIDER_MODE=real (spec 6.13.3).
type VoiceCheckRepo struct{ q *sqlcgen.Queries }

var (
	_ content.VoiceChecker          = (*VoiceCheckRepo)(nil)
	_ content.VendorCharacterLister = (*VoiceCheckRepo)(nil)
)

// NewVoiceCheckRepo returns the voice checker over the pool.
func NewVoiceCheckRepo(pool *pgxpool.Pool) *VoiceCheckRepo {
	return &VoiceCheckRepo{q: sqlcgen.New(pool)}
}

// PlaceholderVoices lists the voice slots of ACTIVE characters of vendors that still hold a
// PLACEHOLDER_ voice_id or model_name (primary and voice.fallback).
func (r *VoiceCheckRepo) PlaceholderVoices(ctx context.Context, vendors []string) ([]content.PlaceholderVoice, error) {
	if vendors == nil {
		vendors = []string{}
	}
	rows, err := r.q.PlaceholderVoices(ctx, vendors)
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeInternalError, "placeholder voices query failed")
	}
	out := make([]content.PlaceholderVoice, 0, len(rows))
	for _, v := range rows {
		out = append(out, content.PlaceholderVoice{
			CharacterCode: v.CharacterCode, Slot: v.Slot, Provider: v.Provider,
			PlaceholderVoiceID: v.PlaceholderVoiceID, PlaceholderModel: v.PlaceholderModel,
		})
	}
	return out, nil
}

// ActiveCharactersOutside lists the ACTIVE characters whose primary vendor is not in enabled.
func (r *VoiceCheckRepo) ActiveCharactersOutside(ctx context.Context, enabled []string) ([]content.VendorCharacter, error) {
	if enabled == nil {
		enabled = []string{}
	}
	rows, err := r.q.ActiveCharactersOutsideVendors(ctx, enabled)
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeInternalError, "characters outside enabled vendors query failed")
	}
	out := make([]content.VendorCharacter, 0, len(rows))
	for _, v := range rows {
		out = append(out, content.VendorCharacter{CharacterCode: v.CharacterCode, Provider: v.Provider})
	}
	return out, nil
}
