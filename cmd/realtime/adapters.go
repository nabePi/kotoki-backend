package main

import (
	"context"
	"errors"

	"github.com/google/uuid"

	"github.com/nabePi/kotoki-backend/internal/content"
	"github.com/nabePi/kotoki-backend/internal/session"
)

// publishedPrompts is the part of content.Repository the opening-prompt adapter reads.
type publishedPrompts interface {
	GetPublishedPromptVersion(ctx context.Context, key content.PromptKey) (content.PromptVersion, error)
}

// openingPromptSource adapts the content repository to session.OpeningPromptSource: the PUBLISHED
// version id of a prompt key, or nil when there is none (S-2). session.Service.BeginStart (the
// realtime process, AI_FIRST and USER_FIRST alike) reads it before the STARTING transaction, so
// the repository is read directly, not through the API's content cache: a retired OPENING version
// must take effect on the next start.
type openingPromptSource struct{ repo publishedPrompts }

var _ session.OpeningPromptSource = openingPromptSource{}

func (s openingPromptSource) PublishedPromptVersionID(ctx context.Context, key string) (*uuid.UUID, error) {
	pv, err := s.repo.GetPublishedPromptVersion(ctx, content.PromptKey(key))
	if errors.Is(err, content.ErrNotFound) {
		return nil, nil //nolint:nilnil // "no PUBLISHED version" is a normal answer, not an error
	}
	if err != nil {
		return nil, err
	}
	id := pv.ID
	return &id, nil
}
