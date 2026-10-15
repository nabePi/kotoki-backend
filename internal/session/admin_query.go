package session

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"

	"github.com/nabePi/kotoki-backend/internal/balance"
	"github.com/nabePi/kotoki-backend/internal/platform/clock"
)

// redacted replaces the vendor details of a character in what admins read (D-015, spec 5.9.8): the
// voice id is treated as sensitive, the persona stays visible. The real values live only in the DB.
const redacted = "[REDACTED]"

// AdminView is a session as admins read it: everything of the user view plus the internal fields
// (the embedded Session carries user_id, versions, runtime_fence, handoff_at, adaptive_difficulty …)
// and the reservation. Session.ConfigSnapshot is the redacted copy.
type AdminView struct {
	View
	Reservation *balance.Reservation
}

// EventCursor is the keyset position in session_events: (created_at, id) of the last event seen.
type EventCursor struct {
	CreatedAt time.Time
	ID        uuid.UUID
}

// AdminRepository is the admin read port of sessions: no owner filter, reservation joined in, every
// turn and every event.
type AdminRepository interface {
	GetAdminView(ctx context.Context, id uuid.UUID) (AdminView, error)
	ListAdminViews(ctx context.Context, userID uuid.UUID, statuses []Status, after *uuid.UUID, limit int) ([]AdminView, error)
	// ListAllTurns returns every turn, DISCARDED and in-progress included, bounded by created_at
	// like TurnRepository.ListForUser.
	ListAllTurns(ctx context.Context, sessionID uuid.UUID, startingAt time.Time, endedAt *time.Time, afterSeq *int, limit int) ([]TurnRecord, error)
	// ListEvents orders by (created_at, id) ascending, strictly after the cursor.
	ListEvents(ctx context.Context, sessionID uuid.UUID, after *EventCursor, limit int) ([]Event, error)
}

// AdminQueryService is the admin read side (spec 5.9.8). Writing the SESSION_TRANSCRIPT_VIEW audit
// row for a transcript read is the caller's job, after this service succeeded.
type AdminQueryService struct {
	repo          AdminRepository
	clk           clock.Clock
	retentionDays int
}

// NewAdminQueryService builds the service; retentionDays is the transcript retention (D-099).
func NewAdminQueryService(r AdminRepository, c clock.Clock, retentionDays int) *AdminQueryService {
	return &AdminQueryService{repo: r, clk: c, retentionDays: retentionDays}
}

// RedactSnapshot returns config_snapshot with the character's provider_name, model_name, voice_id and
// voice.fallback replaced by "[REDACTED]"; everything else is kept. A snapshot that is not a JSON
// object is returned as it is.
func RedactSnapshot(raw json.RawMessage) json.RawMessage {
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return raw
	}
	if ch, ok := doc["character"].(map[string]any); ok {
		for _, k := range []string{"provider_name", "model_name", "voice_id"} {
			if _, has := ch[k]; has {
				ch[k] = redacted
			}
		}
		if voice, ok := ch["voice"].(map[string]any); ok {
			if fb, has := voice["fallback"]; has && fb != nil {
				voice["fallback"] = redacted
			}
		}
	}
	out, err := json.Marshal(doc)
	if err != nil {
		return raw
	}
	return out
}

func redactView(v AdminView) AdminView {
	v.ConfigSnapshot = RedactSnapshot(v.ConfigSnapshot)
	return v
}

// ListByUser lists a user's sessions newest first, for any user.
func (a *AdminQueryService) ListByUser(ctx context.Context, userID uuid.UUID, statuses []Status, after *uuid.UUID, limit int) ([]AdminView, error) {
	rows, err := a.repo.ListAdminViews(ctx, userID, statuses, after, limit)
	if err != nil {
		return nil, err
	}
	for i := range rows {
		rows[i] = redactView(rows[i])
	}
	return rows, nil
}

// Get returns any session; a missing one is SESSION_NOT_FOUND.
func (a *AdminQueryService) Get(ctx context.Context, id uuid.UUID) (AdminView, error) {
	v, err := a.repo.GetAdminView(ctx, id)
	if err != nil {
		return AdminView{}, mapNotFound(err)
	}
	return redactView(v), nil
}

// Turns pages every turn of a session; the retention rules are the same as for the user (410).
func (a *AdminQueryService) Turns(ctx context.Context, id uuid.UUID, afterSeq *int, limit int) ([]TurnRecord, error) {
	v, err := a.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if v.TranscriptPurgedAt != nil {
		return nil, transcriptExpired()
	}
	if v.EndedAt != nil && a.clk.Now().Sub(*v.EndedAt) > time.Duration(a.retentionDays)*24*time.Hour {
		return nil, transcriptExpired()
	}
	if v.StartingAt == nil {
		return []TurnRecord{}, nil
	}
	out, err := a.repo.ListAllTurns(ctx, id, *v.StartingAt, v.EndedAt, afterSeq, limit)
	if err != nil {
		return nil, err
	}
	if out == nil {
		out = []TurnRecord{}
	}
	return out, nil
}

// Events pages the session_events timeline of a session.
func (a *AdminQueryService) Events(ctx context.Context, id uuid.UUID, after *EventCursor, limit int) ([]Event, error) {
	if _, err := a.Get(ctx, id); err != nil {
		return nil, err
	}
	out, err := a.repo.ListEvents(ctx, id, after, limit)
	if err != nil {
		return nil, err
	}
	if out == nil {
		out = []Event{}
	}
	return out, nil
}
