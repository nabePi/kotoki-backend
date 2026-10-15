package content

import (
	"context"
	"errors"

	"github.com/nabePi/kotoki-backend/internal/platform/errs"
)

type configLoader struct{ repo Repository }

// NewSessionConfigLoader loads session configuration by id. It applies no status filter and
// never reads the live characters row: the character comes from the session snapshot.
func NewSessionConfigLoader(r Repository) SessionConfigLoader { return &configLoader{repo: r} }

func loadErr(err error, what string) error {
	if errors.Is(err, ErrNotFound) {
		return errs.Wrap(err, errs.CodeInternalError, what+" of the session no longer exists")
	}
	return errs.Wrap(err, errs.CodeInternalError, "load "+what+" failed")
}

// Load implements SessionConfigLoader.
func (l *configLoader) Load(ctx context.Context, refs SessionRefs) (SessionConfig, error) {
	snap, err := ParseSnapshot(refs.ConfigSnapshot)
	if err != nil {
		return SessionConfig{}, errs.Wrap(err, errs.CodeInternalError, "session config snapshot is invalid")
	}
	topic, err := l.repo.GetTopicByID(ctx, refs.TopicID)
	if err != nil {
		return SessionConfig{}, loadErr(err, "topic")
	}
	sv, err := l.repo.GetScenarioVersion(ctx, refs.ScenarioVersionID)
	if err != nil {
		return SessionConfig{}, loadErr(err, "scenario version")
	}
	pv, err := l.repo.GetPromptVersion(ctx, refs.PromptVersionID)
	if err != nil {
		return SessionConfig{}, loadErr(err, "prompt version")
	}
	return SessionConfig{Topic: topic, ScenarioVersion: sv, Snapshot: snap, PromptVersion: pv}, nil
}
