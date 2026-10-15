package fakes

import (
	"context"
	"sort"
	"time"

	"github.com/google/uuid"

	"github.com/nabePi/kotoki-backend/internal/session"
)

var _ session.AdminRepository = (*SessionAdmin)(nil)

// SessionAdmin is a session.AdminRepository over a SessionStore: no owner filter, the reservation
// joined in, every turn and the event timeline ordered by (created_at, id).
type SessionAdmin struct {
	views *SessionViews
	store *SessionStore
}

// NewSessionAdmin returns the admin reader.
func NewSessionAdmin(store *SessionStore, content *ContentRepo) *SessionAdmin {
	return &SessionAdmin{views: NewSessionViews(store, content), store: store}
}

func (a *SessionAdmin) adminView(ctx context.Context, s session.Session) session.AdminView {
	out := session.AdminView{View: a.views.view(ctx, s)}
	if res := a.store.bal.Reservation(s.ID); res.ID != uuid.Nil {
		out.Reservation = &res
	}
	return out
}

// GetAdminView implements session.AdminRepository.
func (a *SessionAdmin) GetAdminView(ctx context.Context, id uuid.UUID) (session.AdminView, error) {
	s := a.store.Session(id)
	if s.ID == uuid.Nil {
		return session.AdminView{}, session.ErrNotFound
	}
	return a.adminView(ctx, s), nil
}

// ListAdminViews implements session.AdminRepository.
func (a *SessionAdmin) ListAdminViews(ctx context.Context, userID uuid.UUID, statuses []session.Status, after *uuid.UUID, limit int) ([]session.AdminView, error) {
	list, err := a.views.ListViews(ctx, userID, statuses, after, limit)
	if err != nil {
		return nil, err
	}
	out := make([]session.AdminView, len(list))
	for i, v := range list {
		out[i] = a.adminView(ctx, v.Session)
	}
	return out, nil
}

// ListAllTurns implements session.AdminRepository.
func (a *SessionAdmin) ListAllTurns(_ context.Context, sessionID uuid.UUID, _ time.Time, _ *time.Time, afterSeq *int, limit int) ([]session.TurnRecord, error) {
	var out []session.Turn
	for _, t := range a.store.Turns(sessionID) {
		if afterSeq != nil && t.Sequence <= *afterSeq {
			continue
		}
		out = append(out, t)
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// ListEvents implements session.AdminRepository.
func (a *SessionAdmin) ListEvents(_ context.Context, sessionID uuid.UUID, after *session.EventCursor, limit int) ([]session.Event, error) {
	all := a.store.Events(sessionID)
	sort.SliceStable(all, func(i, j int) bool {
		if !all[i].CreatedAt.Equal(all[j].CreatedAt) {
			return all[i].CreatedAt.Before(all[j].CreatedAt)
		}
		return all[i].ID.String() < all[j].ID.String()
	})
	var out []session.Event
	for _, e := range all {
		if after != nil && (e.CreatedAt.Before(after.CreatedAt) ||
			(e.CreatedAt.Equal(after.CreatedAt) && e.ID.String() <= after.ID.String())) {
			continue
		}
		out = append(out, e)
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}
