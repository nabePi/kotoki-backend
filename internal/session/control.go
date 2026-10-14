package session

import (
	"context"

	"github.com/google/uuid"
)

// ControlMessage is published on kotoki:session:{id}:control after a transition to ENDING or a
// terminal status (spec 5.6.6).
type ControlMessage struct {
	Type      string `json:"type"` // "status_changed"
	SessionID string `json:"session_id"`
	Status    string `json:"status"`
	EndReason string `json:"end_reason,omitempty"`
	Version   int64  `json:"version"`
}

func statusChanged(s Session) ControlMessage {
	m := ControlMessage{Type: "status_changed", SessionID: s.ID.String(), Status: string(s.Status), Version: s.Version}
	if s.EndReason != nil {
		m.EndReason = string(*s.EndReason)
	}
	return m
}

// publishControl tells the owning runtime about a transition, best effort: after the commit, and an
// error is only logged (the runtime also notices through its next fenced call).
func (s *Service) publishControl(ctx context.Context, sess Session) {
	if err := s.d.Control.Publish(ctx, sess.ID, statusChanged(sess)); err != nil {
		s.d.Log.WarnContext(ctx, "session control publish failed", "session_id", sess.ID.String(), "status", string(sess.Status), "error", err.Error())
	}
}

type noControl struct{}

func (noControl) Publish(context.Context, uuid.UUID, ControlMessage) error { return nil }
