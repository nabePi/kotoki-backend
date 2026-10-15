package content

import (
	"context"
	"encoding/json"

	"github.com/google/uuid"
)

// ResolveInput is the client part of POST /sessions (spec 6.9). The client never sends version
// ids, voice, style, provider or model; such fields are ignored upstream.
type ResolveInput struct {
	UserID                uuid.UUID
	TopicID               uuid.UUID
	ScenarioID            uuid.UUID
	CharacterID           uuid.UUID
	ConversationStartMode string // USER_FIRST | AI_FIRST
}

// ResolvedConfig is what session creation snapshots into the sessions row (D-064).
type ResolvedConfig struct {
	TopicID           uuid.UUID
	ScenarioID        uuid.UUID
	ScenarioVersionID uuid.UUID
	CharacterID       uuid.UUID
	PromptVersionID   uuid.UUID       // key CONVERSATION
	ConfigSnapshot    SessionSnapshot // → sessions.config_snapshot
}

// Resolver turns a session request into a ResolvedConfig or a content error.
type Resolver interface {
	Resolve(ctx context.Context, in ResolveInput) (ResolvedConfig, error)
}

// SessionRefs are the ids and the snapshot stored on a session row.
type SessionRefs struct {
	TopicID           uuid.UUID
	ScenarioVersionID uuid.UUID
	PromptVersionID   uuid.UUID
	ConfigSnapshot    json.RawMessage // sessions.config_snapshot
}

// SessionConfig is everything the realtime runtime needs for a session.
type SessionConfig struct {
	Topic           Topic
	ScenarioVersion ScenarioVersion
	Snapshot        SessionSnapshot // the character comes from here, never from the live row
	PromptVersion   PromptVersion   // CONVERSATION
}

// SessionConfigLoader loads a session's configuration by id, without any status filter: a
// version retired or a scenario disabled after the session was created must not break it.
type SessionConfigLoader interface {
	Load(ctx context.Context, refs SessionRefs) (SessionConfig, error)
}

// Repository is the read port of content (spec 6.5). Missing rows are ErrNotFound. Methods
// that take an entity id and return the row regardless of status are the "by id" family
// (cached without TTL: versions are immutable once published); the others are the "live"
// family (cached for CONTENT_CACHE_TTL_SECONDS).
type Repository interface {
	// live
	GetTopic(ctx context.Context, id uuid.UUID) (Topic, error)
	GetScenario(ctx context.Context, id uuid.UUID) (Scenario, error)
	GetPublishedScenarioVersion(ctx context.Context, scenarioID uuid.UUID) (ScenarioVersion, error)
	GetCharacter(ctx context.Context, id uuid.UUID) (Character, error)
	GetPublishedPromptVersion(ctx context.Context, key PromptKey) (PromptVersion, error)
	// ListTopics, ListScenarios (of one topic) and ListCharacters return ACTIVE rows ordered by
	// sort_order, code (at most 200).
	ListTopics(ctx context.Context) ([]Topic, error)
	ListScenarios(ctx context.Context, topicID uuid.UUID) ([]Scenario, error)
	ListCharacters(ctx context.Context) ([]Character, error)
	ListPublishedScenarioVersions(ctx context.Context) ([]ScenarioVersion, error)
	ListActiveCharacters(ctx context.Context) ([]Character, error)
	ListPublishedPromptVersions(ctx context.Context) ([]PromptVersion, error)
	// by id (any status)
	GetScenarioVersion(ctx context.Context, id uuid.UUID) (ScenarioVersion, error)
	GetPromptVersion(ctx context.Context, id uuid.UUID) (PromptVersion, error)
	GetTopicByID(ctx context.Context, id uuid.UUID) (Topic, error)
}

var _ ValidatorRepo = Repository(nil)
