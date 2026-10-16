package fakes

import (
	"context"
	"sync"

	"github.com/google/uuid"

	"github.com/nabePi/kotoki-backend/internal/learner"
)

var _ learner.UserGate = (*LearnerUserGate)(nil)

// LearnerUserGate is a learner.UserGate whose answer tests set per user; unknown users are not
// deleted.
type LearnerUserGate struct {
	FailInjector
	mu      sync.Mutex
	deleted map[uuid.UUID]bool
}

// NewLearnerUserGate returns a gate that reports nobody as deleted.
func NewLearnerUserGate() *LearnerUserGate { return &LearnerUserGate{deleted: map[uuid.UUID]bool{}} }

// MarkDeleted makes UserDeleted report userID as deleted.
func (g *LearnerUserGate) MarkDeleted(userID uuid.UUID) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.deleted[userID] = true
}

// UserDeleted implements learner.UserGate.
func (g *LearnerUserGate) UserDeleted(_ context.Context, userID uuid.UUID) (bool, error) {
	if err := g.Take("UserDeleted"); err != nil {
		return false, err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.deleted[userID], nil
}
