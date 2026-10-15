package fakes

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/nabePi/kotoki-backend/internal/auth"
)

// TicketSessionReader is a configurable auth.TicketSessionReader.
type TicketSessionReader struct {
	FailInjector
	mu      sync.Mutex
	targets map[[2]uuid.UUID]auth.TicketTarget
}

var _ auth.TicketSessionReader = (*TicketSessionReader)(nil)

// NewTicketSessionReader returns a reader that knows no sessions.
func NewTicketSessionReader() *TicketSessionReader {
	return &TicketSessionReader{targets: map[[2]uuid.UUID]auth.TicketTarget{}}
}

// Set registers the target returned for (sessionID, userID).
func (r *TicketSessionReader) Set(sessionID, userID uuid.UUID, t auth.TicketTarget) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.targets[[2]uuid.UUID{sessionID, userID}] = t
}

// GetTicketTarget returns the registered target or auth.ErrSessionNotFound.
func (r *TicketSessionReader) GetTicketTarget(_ context.Context, sessionID, userID uuid.UUID) (auth.TicketTarget, error) {
	if err := r.Take("GetTicketTarget"); err != nil {
		return auth.TicketTarget{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	t, ok := r.targets[[2]uuid.UUID{sessionID, userID}]
	if !ok {
		return auth.TicketTarget{}, auth.ErrSessionNotFound
	}
	return t, nil
}

// TicketStore is an in-memory auth.TicketStore (single use, no expiry clock).
type TicketStore struct {
	FailInjector
	mu      sync.Mutex
	tickets map[string]auth.WSTicket
	Issued  []auth.WSTicket
	TTLs    []time.Duration
}

var _ auth.TicketStore = (*TicketStore)(nil)

// NewTicketStore returns an empty store.
func NewTicketStore() *TicketStore { return &TicketStore{tickets: map[string]auth.WSTicket{}} }

// Issue stores t under a random 43-char ticket.
func (s *TicketStore) Issue(_ context.Context, t auth.WSTicket, ttl time.Duration) (string, error) {
	if err := s.Take("Issue"); err != nil {
		return "", err
	}
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	raw := base64.RawURLEncoding.EncodeToString(b)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tickets[raw] = t
	s.Issued = append(s.Issued, t)
	s.TTLs = append(s.TTLs, ttl)
	return raw, nil
}

// Consume returns and deletes the ticket.
func (s *TicketStore) Consume(_ context.Context, plaintext string) (auth.WSTicket, error) {
	if err := s.Take("Consume"); err != nil {
		return auth.WSTicket{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tickets[plaintext]
	if !ok {
		return auth.WSTicket{}, auth.ErrTicketInvalid
	}
	delete(s.tickets, plaintext)
	return t, nil
}
