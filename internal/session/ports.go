package session

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"

	"github.com/nabePi/kotoki-backend/internal/content"
)

// UserGateInfo is what session creation needs to know about the user (spec 4.6.1 step 1).
type UserGateInfo struct {
	Status       string // ACTIVE | SUSPENDED | DELETED …
	TermsVersion string // the terms version the user accepted
}

// UserGate reads the user's status and accepted terms without importing auth.
type UserGate interface {
	Gate(ctx context.Context, userID uuid.UUID) (UserGateInfo, error)
}

// AdaptiveRequest asks the learner side for the adaptive-difficulty snapshot of a new session.
type AdaptiveRequest struct {
	UserID uuid.UUID
	Config content.ResolvedConfig
}

// AdaptiveProvider returns the sessions.adaptive_difficulty JSON object (spec 12.8.2). M8 provides
// the real adapter over learner.Service; M3 uses a fake.
type AdaptiveProvider interface {
	Adaptive(ctx context.Context, req AdaptiveRequest) (json.RawMessage, error)
}

// OpeningPromptSource finds the PUBLISHED version of a prompt key for BeginStart (S-2): nil when
// there is none, which is not an error.
type OpeningPromptSource interface {
	PublishedPromptVersionID(ctx context.Context, key string) (*uuid.UUID, error)
}

// ControlPublisher publishes control messages; best effort, the Service only logs its errors.
type ControlPublisher interface {
	Publish(ctx context.Context, sessionID uuid.UUID, msg ControlMessage) error
}

// HeartbeatReader reads the last runtime heartbeat per session (Redis in production).
type HeartbeatReader interface {
	Read(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]time.Time, error)
}
