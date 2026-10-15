package fakes

import (
	"context"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/nabePi/kotoki-backend/internal/content"
)

var (
	_ content.TxRepository = (*ContentRepo)(nil)
	_ content.Store        = (*ContentStore)(nil)
)

// ContentStore is an in-memory content.Store over a ContentRepo. A transaction takes a snapshot
// of the repository and the audit log and restores it when the function fails, so rollbacks are
// observable; transactions run one at a time (the parent lock of Postgres).
type ContentStore struct {
	Repo *ContentRepo
	mu   sync.Mutex
	// Audit is every committed audit entry, in order.
	audit []content.AuditEntry
}

// NewContentStore returns a store over repo.
func NewContentStore(repo *ContentRepo) *ContentStore { return &ContentStore{Repo: repo} }

// Audit returns the committed audit entries.
func (s *ContentStore) Audit() []content.AuditEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.audit)
}

type contentTx struct {
	repo  *ContentRepo
	audit *[]content.AuditEntry
}

func (t contentTx) Content() content.TxRepository { return t.repo }
func (t contentTx) Audit() content.AuditWriter    { return t }

// Record implements content.AuditWriter.
func (t contentTx) Record(_ context.Context, e content.AuditEntry) error {
	*t.audit = append(*t.audit, e)
	return nil
}

// WithinTx implements content.Store.
func (s *ContentStore) WithinTx(ctx context.Context, fn func(ctx context.Context, tx content.Tx) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	snap := s.Repo.snapshot()
	audit := slices.Clone(s.audit)
	if err := fn(ctx, contentTx{repo: s.Repo, audit: &audit}); err != nil {
		s.Repo.restore(snap)
		return err
	}
	s.audit = audit
	return nil
}

type contentSnapshot struct {
	topics           map[uuid.UUID]content.Topic
	scenarios        map[uuid.UUID]content.Scenario
	scenarioVersions map[uuid.UUID]content.ScenarioVersion
	characters       map[uuid.UUID]content.Character
	promptVersions   map[uuid.UUID]content.PromptVersion
}

func (r *ContentRepo) snapshot() contentSnapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	return contentSnapshot{maps.Clone(r.topics), maps.Clone(r.scenarios), maps.Clone(r.scenarioVersions), maps.Clone(r.characters), maps.Clone(r.promptVersions)}
}

func (r *ContentRepo) restore(s contentSnapshot) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.topics, r.scenarios, r.scenarioVersions, r.characters, r.promptVersions = s.topics, s.scenarios, s.scenarioVersions, s.characters, s.promptVersions
}

// LockParent implements content.TxRepository.
func (r *ContentRepo) LockParent(_ context.Context, fam content.Family, parent string) (uuid.UUID, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch fam {
	case content.FamilyScenarios:
		id, err := uuid.Parse(parent)
		if err != nil {
			return uuid.Nil, content.ErrNotFound
		}
		if _, ok := r.scenarios[id]; !ok {
			return uuid.Nil, content.ErrNotFound
		}
		return id, nil
	case content.FamilyPrompts:
		for _, v := range r.promptVersions {
			if string(v.Key) == strings.ToUpper(parent) {
				return v.PromptConfigID, nil
			}
		}
	}
	return uuid.Nil, content.ErrNotFound
}

// LockVersion implements content.TxRepository.
func (r *ContentRepo) LockVersion(_ context.Context, fam content.Family, parentID, versionID uuid.UUID) (content.VersionState, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch fam {
	case content.FamilyScenarios:
		if v, ok := r.scenarioVersions[versionID]; ok && v.ScenarioID == parentID {
			return content.VersionState{ID: v.ID, Version: v.Version, Status: v.Status}, nil
		}
	case content.FamilyPrompts:
		if v, ok := r.promptVersions[versionID]; ok && v.PromptConfigID == parentID {
			return content.VersionState{ID: v.ID, Version: v.Version, Status: v.Status}, nil
		}
	}
	return content.VersionState{}, content.ErrNotFound
}

// NextVersionNumber implements content.TxRepository.
func (r *ContentRepo) NextVersionNumber(_ context.Context, fam content.Family, parentID uuid.UUID) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	top := 0
	for _, v := range r.scenarioVersions {
		if fam == content.FamilyScenarios && v.ScenarioID == parentID {
			top = max(top, v.Version)
		}
	}
	for _, v := range r.promptVersions {
		if fam == content.FamilyPrompts && v.PromptConfigID == parentID {
			top = max(top, v.Version)
		}
	}
	return top + 1, nil
}

// InsertScenarioVersion implements content.TxRepository.
func (r *ContentRepo) InsertScenarioVersion(_ context.Context, v content.ScenarioVersion) error {
	v.Status = content.VersionDraft
	r.locked(func() { r.scenarioVersions[v.ID] = v })
	return nil
}

// InsertPromptVersion implements content.TxRepository.
func (r *ContentRepo) InsertPromptVersion(_ context.Context, v content.PromptVersion) error {
	v.Status = content.VersionDraft
	r.locked(func() { r.promptVersions[v.ID] = v })
	return nil
}

// RetirePublished implements content.TxRepository.
func (r *ContentRepo) RetirePublished(_ context.Context, fam content.Family, parentID uuid.UUID, at time.Time) (*uuid.UUID, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch fam {
	case content.FamilyScenarios:
		for id, v := range r.scenarioVersions {
			if v.ScenarioID == parentID && v.Status == content.VersionPublished {
				v.Status, v.RetiredAt = content.VersionRetired, &at
				r.scenarioVersions[id] = v
				return &id, nil
			}
		}
	case content.FamilyPrompts:
		for id, v := range r.promptVersions {
			if v.PromptConfigID == parentID && v.Status == content.VersionPublished {
				v.Status, v.RetiredAt = content.VersionRetired, &at
				r.promptVersions[id] = v
				return &id, nil
			}
		}
	}
	return nil, nil
}

// SetPublished implements content.TxRepository.
func (r *ContentRepo) SetPublished(_ context.Context, fam content.Family, id uuid.UUID, at time.Time) error {
	return r.setStatus(fam, id, content.VersionPublished, &at, nil)
}

// SetRetired implements content.TxRepository.
func (r *ContentRepo) SetRetired(_ context.Context, fam content.Family, id uuid.UUID, at time.Time) error {
	return r.setStatus(fam, id, content.VersionRetired, nil, &at)
}

func (r *ContentRepo) setStatus(fam content.Family, id uuid.UUID, to content.VersionStatus, published, retired *time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch fam {
	case content.FamilyScenarios:
		if v, ok := r.scenarioVersions[id]; ok {
			v.Status = to
			if published != nil {
				v.PublishedAt, v.RetiredAt = published, nil
			}
			if retired != nil {
				v.RetiredAt = retired
			}
			r.scenarioVersions[id] = v
			return nil
		}
	case content.FamilyPrompts:
		if v, ok := r.promptVersions[id]; ok {
			v.Status = to
			if published != nil {
				v.PublishedAt, v.RetiredAt = published, nil
			}
			if retired != nil {
				v.RetiredAt = retired
			}
			r.promptVersions[id] = v
			return nil
		}
	}
	return content.ErrNotFound
}

// DeleteDraft implements content.TxRepository.
func (r *ContentRepo) DeleteDraft(_ context.Context, fam content.Family, id uuid.UUID) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch fam {
	case content.FamilyScenarios:
		if v, ok := r.scenarioVersions[id]; ok && v.Status == content.VersionDraft {
			delete(r.scenarioVersions, id)
			return nil
		}
	case content.FamilyPrompts:
		if v, ok := r.promptVersions[id]; ok && v.Status == content.VersionDraft {
			delete(r.promptVersions, id)
			return nil
		}
	}
	return content.ErrNotFound
}

// SetEntityStatus implements content.TxRepository.
func (r *ContentRepo) SetEntityStatus(_ context.Context, entity string, id uuid.UUID, to content.EntityStatus, at time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch entity {
	case "topics":
		if t, ok := r.topics[id]; ok {
			t.Status, t.UpdatedAt = to, at
			r.topics[id] = t
			return nil
		}
	case "scenarios":
		if s, ok := r.scenarios[id]; ok {
			s.Status, s.UpdatedAt = to, at
			r.scenarios[id] = s
			return nil
		}
	}
	return content.ErrNotFound
}

// LockCharacter implements content.TxRepository.
func (r *ContentRepo) LockCharacter(_ context.Context, id uuid.UUID) (content.Character, error) {
	return get(r, r.characters, id)
}

// UpdateCharacter implements content.TxRepository.
func (r *ContentRepo) UpdateCharacter(_ context.Context, c content.Character, at time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.characters[c.ID]; !ok {
		return content.ErrNotFound
	}
	c.UpdatedAt = at
	r.characters[c.ID] = c
	return nil
}

// ListScenarioVersions implements content.TxRepository.
func (r *ContentRepo) ListScenarioVersions(_ context.Context, scenarioID uuid.UUID) ([]content.ScenarioVersion, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []content.ScenarioVersion
	for _, v := range r.scenarioVersions {
		if v.ScenarioID == scenarioID {
			out = append(out, v)
		}
	}
	return out, nil
}

// ListPromptVersions implements content.TxRepository.
func (r *ContentRepo) ListPromptVersions(_ context.Context, key content.PromptKey) ([]content.PromptVersion, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []content.PromptVersion
	for _, v := range r.promptVersions {
		if v.Key == key {
			out = append(out, v)
		}
	}
	return out, nil
}
