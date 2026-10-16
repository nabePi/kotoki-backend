package fakes

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/nabePi/kotoki-backend/internal/memory"
	"github.com/nabePi/kotoki-backend/internal/platform/clock"
	"github.com/nabePi/kotoki-backend/internal/prompt"
	"github.com/nabePi/kotoki-backend/internal/provider"
	"github.com/nabePi/kotoki-backend/internal/provider/fake"
)

var (
	_ memory.Source          = (*MemorySource)(nil)
	_ memory.ProfileSettings = (*MemoryProfile)(nil)
	_ memory.PromptSource    = (*MemoryPrompts)(nil)
	_ memory.Provider        = (*MemoryProvider)(nil)
	_ memory.MemVer          = (*MemVerRecorder)(nil)
)

// MemorySource is a memory.Source over maps the test fills. Existing reads the reader of Store.
type MemorySource struct {
	FailInjector
	mu       sync.Mutex
	deleted  map[uuid.UUID]bool
	sessions map[uuid.UUID]memory.SessionInfo
	turns    map[uuid.UUID][]memory.TurnRow
	// Store backs Existing; nil means no existing memory.
	Store *MemoryStore
	// Clock is the time Existing filters expiry with; nil means the system clock.
	Clock clock.Clock
}

// NewMemorySource returns an empty source.
func NewMemorySource() *MemorySource {
	return &MemorySource{deleted: map[uuid.UUID]bool{}, sessions: map[uuid.UUID]memory.SessionInfo{}, turns: map[uuid.UUID][]memory.TurnRow{}}
}

// MarkDeleted makes UserDeleted report userID as deleted.
func (s *MemorySource) MarkDeleted(userID uuid.UUID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deleted[userID] = true
}

// AddSession registers a session with its turns.
func (s *MemorySource) AddSession(id uuid.UUID, info memory.SessionInfo, turns []memory.TurnRow) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[id] = info
	s.turns[id] = slices.Clone(turns)
}

// UserDeleted implements memory.Source.
func (s *MemorySource) UserDeleted(_ context.Context, userID uuid.UUID) (bool, error) {
	if err := s.Take("UserDeleted"); err != nil {
		return false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.deleted[userID], nil
}

// Session implements memory.Source.
func (s *MemorySource) Session(_ context.Context, id uuid.UUID) (memory.SessionInfo, error) {
	if err := s.Take("Session"); err != nil {
		return memory.SessionInfo{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	info, ok := s.sessions[id]
	if !ok {
		return memory.SessionInfo{}, memory.ErrNotFound
	}
	return info, nil
}

// Turns implements memory.Source.
func (s *MemorySource) Turns(_ context.Context, id uuid.UUID) ([]memory.TurnRow, error) {
	if err := s.Take("Turns"); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.turns[id]), nil
}

// Existing implements memory.Source.
func (s *MemorySource) Existing(ctx context.Context, userID uuid.UUID, limit int) ([]memory.ExistingMemoryLine, error) {
	if err := s.Take("Existing"); err != nil {
		return nil, err
	}
	if s.Store == nil {
		return nil, nil
	}
	return s.Store.Reader().ListExisting(ctx, userID, limit, s.now())
}

func (s *MemorySource) now() time.Time {
	if s.Clock == nil {
		return clock.System{}.Now()
	}
	return s.Clock.Now()
}

// MemoryProfile is a memory.ProfileSettings: memory is enabled unless the test disables a user.
type MemoryProfile struct {
	FailInjector
	mu       sync.Mutex
	disabled map[uuid.UUID]bool
}

// NewMemoryProfile returns settings that enable memory for everyone.
func NewMemoryProfile() *MemoryProfile { return &MemoryProfile{disabled: map[uuid.UUID]bool{}} }

// Disable turns memory off for userID.
func (p *MemoryProfile) Disable(userID uuid.UUID) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.disabled[userID] = true
}

// EnsureMemoryEnabled implements memory.ProfileSettings.
func (p *MemoryProfile) EnsureMemoryEnabled(_ context.Context, userID uuid.UUID) (bool, error) {
	if err := p.Take("EnsureMemoryEnabled"); err != nil {
		return false, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return !p.disabled[userID], nil
}

// MemoryPrompts is a memory.PromptSource holding at most one template per key.
type MemoryPrompts struct {
	FailInjector
	mu   sync.Mutex
	tpl  map[string]prompt.Template
	ids  map[string]uuid.UUID
	Gets int
}

// NewMemoryPrompts returns an empty source: every key is ErrPromptNotFound.
func NewMemoryPrompts() *MemoryPrompts {
	return &MemoryPrompts{tpl: map[string]prompt.Template{}, ids: map[string]uuid.UUID{}}
}

// Set publishes tpl under key with id.
func (p *MemoryPrompts) Set(key string, id uuid.UUID, tpl prompt.Template) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.tpl[key], p.ids[key] = tpl, id
}

// Published implements memory.PromptSource.
func (p *MemoryPrompts) Published(_ context.Context, key string) (prompt.Template, uuid.UUID, error) {
	if err := p.Take("Published"); err != nil {
		return prompt.Template{}, uuid.Nil, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.Gets++
	t, ok := p.tpl[key]
	if !ok {
		return prompt.Template{}, uuid.Nil, memory.ErrPromptNotFound
	}
	return t, p.ids[key], nil
}

// MemoryReply is one scripted answer of MemoryProvider.GenerateJSON.
type MemoryReply struct {
	JSON string
	Err  error
}

// MemoryProvider is a scriptable memory.Provider. GenerateJSON answers the scripted replies in
// order (the test fails loudly through an error when they run out); Embed is the fake embedding
// provider of M4 (deterministic 1536-dim vectors). FailNext("Embed", err) fails the next embedding.
type MemoryProvider struct {
	FailInjector
	mu       sync.Mutex
	replies  []MemoryReply
	requests []provider.StructuredRequest
	embeds   []provider.EmbeddingRequest
	emb      *fake.Embedding
	// Name and Model label the results.
	Name  provider.Name
	Model string
}

// NewMemoryProvider returns a provider that answers replies in order.
func NewMemoryProvider(replies ...MemoryReply) *MemoryProvider {
	return &MemoryProvider{
		replies: replies, Name: "fake", Model: "fake-evaluation",
		emb: fake.NewEmbedding("fake", fake.CapConfig{}, fake.Config{}, fake.Deps{}),
	}
}

// GenerateJSON implements memory.Provider.
func (p *MemoryProvider) GenerateJSON(_ context.Context, req provider.StructuredRequest) (provider.StructuredResult, error) {
	p.mu.Lock()
	p.requests = append(p.requests, req)
	p.mu.Unlock()
	if err := p.Take("GenerateJSON"); err != nil {
		return provider.StructuredResult{}, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.replies) == 0 {
		return provider.StructuredResult{}, fmt.Errorf("fakes: MemoryProvider has no scripted reply for call %d", len(p.requests))
	}
	r := p.replies[0]
	p.replies = p.replies[1:]
	if r.Err != nil {
		return provider.StructuredResult{}, r.Err
	}
	return provider.StructuredResult{JSON: []byte(r.JSON), FinishReason: provider.FinishStop, Provider: p.Name, Model: p.Model}, nil
}

// Embed implements memory.Provider.
func (p *MemoryProvider) Embed(ctx context.Context, req provider.EmbeddingRequest) (provider.EmbeddingResult, error) {
	p.mu.Lock()
	p.embeds = append(p.embeds, req)
	p.mu.Unlock()
	if err := p.Take("Embed"); err != nil {
		return provider.EmbeddingResult{}, err
	}
	return p.emb.Embed(ctx, req)
}

// Requests returns the structured requests received, in order.
func (p *MemoryProvider) Requests() []provider.StructuredRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.requests)
}

// EmbedRequests returns the embedding requests received, in order.
func (p *MemoryProvider) EmbedRequests() []provider.EmbeddingRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.embeds)
}

// MemVerRecorder is a memory.MemVer that records the bumped users; it also acts as an in-memory
// version counter.
type MemVerRecorder struct {
	FailInjector
	mu      sync.Mutex
	bumps   [][]uuid.UUID
	version map[uuid.UUID]int64
}

// NewMemVerRecorder returns an empty recorder.
func NewMemVerRecorder() *MemVerRecorder { return &MemVerRecorder{version: map[uuid.UUID]int64{}} }

// Bump implements memory.MemVer.
func (m *MemVerRecorder) Bump(_ context.Context, userIDs []uuid.UUID) error {
	if err := m.Take("Bump"); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.bumps = append(m.bumps, slices.Clone(userIDs))
	for _, u := range userIDs {
		m.version[u]++
	}
	return nil
}

// Bumps returns the calls made, in order.
func (m *MemVerRecorder) Bumps() [][]uuid.UUID {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.bumps)
}

// Version is the number of times userID was bumped.
func (m *MemVerRecorder) Version(userID uuid.UUID) int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.version[userID]
}
