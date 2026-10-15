package fakes

import (
	"context"
	"sync"

	"github.com/google/uuid"

	"github.com/nabePi/kotoki-backend/internal/memory"
)

var _ memory.RetrievalService = (*MemoryRetrieval)(nil)

// MemoryRetrieval is a scriptable memory.RetrievalService. FailNext("BuildSessionContext" |
// "RefreshEpisodic" | "ReloadCoreIfChanged", err) fails the next call of a method.
type MemoryRetrieval struct {
	FailInjector
	mu sync.Mutex
	// Context is answered by BuildSessionContext (SessionID is filled in); Episodic by RefreshEpisodic.
	Context  memory.MemoryContext
	Episodic []memory.ContextItem
	// CoreChanged makes ReloadCoreIfChanged report a change and replace mc.Core with NewCore.
	CoreChanged bool
	NewCore     []memory.ContextItem

	built     []memory.SessionContextInput
	refreshed [][]memory.TurnText
	reloads   int
}

// BuildSessionContext implements memory.RetrievalService.
func (m *MemoryRetrieval) BuildSessionContext(_ context.Context, in memory.SessionContextInput) (memory.MemoryContext, error) {
	if err := m.Take("BuildSessionContext"); err != nil {
		return memory.MemoryContext{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.built = append(m.built, in)
	mc := m.Context
	mc.SessionID = in.SessionID
	return mc, nil
}

// RefreshEpisodic implements memory.RetrievalService.
func (m *MemoryRetrieval) RefreshEpisodic(_ context.Context, _, _ uuid.UUID, recent []memory.TurnText) ([]memory.ContextItem, error) {
	if err := m.Take("RefreshEpisodic"); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.refreshed = append(m.refreshed, append([]memory.TurnText(nil), recent...))
	return append([]memory.ContextItem(nil), m.Episodic...), nil
}

// ReloadCoreIfChanged implements memory.RetrievalService.
func (m *MemoryRetrieval) ReloadCoreIfChanged(_ context.Context, mc *memory.MemoryContext) (bool, error) {
	if err := m.Take("ReloadCoreIfChanged"); err != nil {
		return false, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.reloads++
	if !m.CoreChanged {
		return false, nil
	}
	mc.Core = append([]memory.ContextItem(nil), m.NewCore...)
	return true, nil
}

// Builds returns the inputs BuildSessionContext saw.
func (m *MemoryRetrieval) Builds() []memory.SessionContextInput {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]memory.SessionContextInput(nil), m.built...)
}

// Refreshes returns the number of RefreshEpisodic calls.
func (m *MemoryRetrieval) Refreshes() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.refreshed)
}

// Reloads returns the number of ReloadCoreIfChanged calls.
func (m *MemoryRetrieval) Reloads() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.reloads
}
