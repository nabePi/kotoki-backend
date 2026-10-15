package fakes

import (
	"context"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/nabePi/kotoki-backend/internal/session"
)

var _ session.HeartbeatReader = (*Heartbeats)(nil)

// Heartbeats is an in-memory session.HeartbeatReader: tests set the last heartbeat per session,
// a session without one has no key. FailNext("Read", err) makes the next Read fail, empty probes
// included.
type Heartbeats struct {
	FailInjector
	mu    sync.Mutex
	beats map[uuid.UUID]time.Time
	// Down makes every Read fail until it is set back to false (Redis unreachable).
	Down bool
	// Reads counts Read calls with at least one id.
	Reads int
}

// NewHeartbeats returns an empty store.
func NewHeartbeats() *Heartbeats { return &Heartbeats{beats: map[uuid.UUID]time.Time{}} }

// Beat records the last heartbeat of a session.
func (h *Heartbeats) Beat(id uuid.UUID, at time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.beats[id] = at
}

// SetDown switches the simulated Redis outage on or off.
func (h *Heartbeats) SetDown(down bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.Down = down
}

// Read implements session.HeartbeatReader.
func (h *Heartbeats) Read(_ context.Context, ids []uuid.UUID) (map[uuid.UUID]time.Time, error) {
	if err := h.Take("Read"); err != nil {
		return nil, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.Down {
		return nil, context.DeadlineExceeded
	}
	out := map[uuid.UUID]time.Time{}
	if len(ids) > 0 {
		h.Reads++
	}
	for _, id := range ids {
		if at, ok := h.beats[id]; ok {
			out[id] = at
		}
	}
	return out, nil
}
