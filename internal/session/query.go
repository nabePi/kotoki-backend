package session

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/nabePi/kotoki-backend/internal/platform/clock"
	"github.com/nabePi/kotoki-backend/internal/platform/errs"
)

// View is a session as the REST layer shows it: the row plus names that do not live on it. The
// character name and avatar come from config_snapshot, so a later edit or disabling of the live
// character never changes how an existing session reads.
type View struct {
	Session
	TopicName       string
	ScenarioName    string
	CharacterName   string
	AvatarKey       *string
	StartDeadlineAt time.Time // of the reservation; zero when the session has none
}

// ViewRepository reads views; a session of another user does not exist for the caller (ErrNotFound).
type ViewRepository interface {
	GetView(ctx context.Context, userID, id uuid.UUID) (View, error)
	ListViews(ctx context.Context, userID uuid.UUID, statuses []Status, after *uuid.UUID, limit int) ([]View, error)
}

// QueryService is the read side of REST: views, lists and the transcript. It never writes.
type QueryService struct {
	views         ViewRepository
	turns         TurnRepository
	clk           clock.Clock
	retentionDays int
}

// NewQueryService builds the service; retentionDays is the transcript retention (D-099, 180).
func NewQueryService(v ViewRepository, t TurnRepository, c clock.Clock, retentionDays int) *QueryService {
	return &QueryService{views: v, turns: t, clk: c, retentionDays: retentionDays}
}

// Get returns one session of the user; anyone else's is 404.
func (q *QueryService) Get(ctx context.Context, userID, id uuid.UUID) (View, error) {
	v, err := q.views.GetView(ctx, userID, id)
	if err != nil {
		return View{}, mapNotFound(err)
	}
	return v, nil
}

// List returns the user's sessions newest first (id DESC), filtered by status; after is the keyset
// cursor. The caller asks for limit+1 rows to learn whether there is a next page.
func (q *QueryService) List(ctx context.Context, userID uuid.UUID, statuses []Status, after *uuid.UUID, limit int) ([]View, error) {
	return q.views.ListViews(ctx, userID, statuses, after, limit)
}

// transcriptExpired is the 410 of GET /sessions/{id}/turns.
func transcriptExpired() error {
	return errs.New(errs.CodeTranscriptExpired, "the transcript of this session is no longer available")
}

// Turns pages the transcript of a session of the user: completed, interrupted and failed turns in
// sequence order (DISCARDED and in-progress ones never show). A session that has not started has
// none; one past the retention (or already purged) is 410. The turn query is bounded by created_at
// for partition pruning (spec 5.3.2).
func (q *QueryService) Turns(ctx context.Context, userID, id uuid.UUID, afterSeq *int, limit int) ([]TurnRecord, error) {
	v, err := q.Get(ctx, userID, id)
	if err != nil {
		return nil, err
	}
	if v.TranscriptPurgedAt != nil {
		return nil, transcriptExpired()
	}
	if v.EndedAt != nil && q.clk.Now().Sub(*v.EndedAt) > time.Duration(q.retentionDays)*24*time.Hour {
		return nil, transcriptExpired()
	}
	if v.StartingAt == nil {
		return []TurnRecord{}, nil
	}
	out, err := q.turns.ListForUser(ctx, id, *v.StartingAt, v.EndedAt, afterSeq, limit)
	if err != nil {
		return nil, err
	}
	if out == nil {
		out = []TurnRecord{}
	}
	return out, nil
}
