package fakes

import (
	"context"
	"encoding/json"
	"sort"

	"github.com/google/uuid"

	"github.com/nabePi/kotoki-backend/internal/session"
)

var _ session.ViewRepository = (*SessionViews)(nil)

// SessionViews is a session.ViewRepository over a SessionStore. Topic and scenario names come from
// the content repository, the character from the session's own snapshot.
type SessionViews struct {
	store   *SessionStore
	content *ContentRepo
}

// NewSessionViews returns the view reader.
func NewSessionViews(store *SessionStore, content *ContentRepo) *SessionViews {
	return &SessionViews{store: store, content: content}
}

func (v *SessionViews) view(ctx context.Context, s session.Session) session.View {
	out := session.View{Session: s}
	if t, err := v.content.GetTopicByID(ctx, s.TopicID); err == nil {
		out.TopicName = t.Name
	}
	if sc, err := v.content.GetScenario(ctx, s.ScenarioID); err == nil {
		out.ScenarioName = sc.Name
	}
	var snap struct {
		Character struct {
			Name      string  `json:"name"`
			AvatarKey *string `json:"avatar_key"`
		} `json:"character"`
	}
	if json.Unmarshal(s.ConfigSnapshot, &snap) == nil {
		out.CharacterName, out.AvatarKey = snap.Character.Name, snap.Character.AvatarKey
	}
	out.StartDeadlineAt = v.store.bal.Reservation(s.ID).StartDeadlineAt
	return out
}

// GetView implements session.ViewRepository.
func (v *SessionViews) GetView(ctx context.Context, userID, id uuid.UUID) (session.View, error) {
	s := v.store.Session(id)
	if s.ID == uuid.Nil || s.UserID != userID {
		return session.View{}, session.ErrNotFound
	}
	return v.view(ctx, s), nil
}

// ListViews implements session.ViewRepository.
func (v *SessionViews) ListViews(ctx context.Context, userID uuid.UUID, statuses []session.Status, after *uuid.UUID, limit int) ([]session.View, error) {
	list, err := v.store.Reader().ListByUser(ctx, userID, session.ListFilter{Statuses: statuses}, after, limit)
	if err != nil {
		return nil, err
	}
	sort.Slice(list, func(i, j int) bool { return list[i].ID.String() > list[j].ID.String() })
	out := make([]session.View, len(list))
	for i, s := range list {
		out[i] = v.view(ctx, s)
	}
	return out, nil
}
