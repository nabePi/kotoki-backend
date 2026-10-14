package fakes

import (
	"context"
	"sync"

	"github.com/google/uuid"

	"github.com/nabePi/kotoki-backend/internal/session"
)

var _ session.UserGate = (*UserGate)(nil)

// UserGate is a session.UserGate whose answer tests set per user; unknown users are ACTIVE with
// the default terms version.
type UserGate struct {
	FailInjector
	mu      sync.Mutex
	Default session.UserGateInfo
	users   map[uuid.UUID]session.UserGateInfo
}

// NewUserGate returns a gate that answers info for every user until Set overrides it.
func NewUserGate(info session.UserGateInfo) *UserGate {
	return &UserGate{Default: info, users: map[uuid.UUID]session.UserGateInfo{}}
}

// Set overrides the answer for one user.
func (g *UserGate) Set(userID uuid.UUID, info session.UserGateInfo) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.users[userID] = info
}

// Gate implements session.UserGate.
func (g *UserGate) Gate(_ context.Context, userID uuid.UUID) (session.UserGateInfo, error) {
	if err := g.Take("Gate"); err != nil {
		return session.UserGateInfo{}, err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if v, ok := g.users[userID]; ok {
		return v, nil
	}
	return g.Default, nil
}
