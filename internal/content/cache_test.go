package content_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/content"
	"github.com/nabePi/kotoki-backend/internal/platform/clock"
	"github.com/nabePi/kotoki-backend/internal/platform/errs"
)

func TestCache(t *testing.T) {
	w := newWorld()
	clk := clock.NewFake(t0)
	c := content.NewCachedRepository(w.repo, 30*time.Second, clk)
	ctx := context.Background()

	for i := 0; i < 2; i++ {
		_, err := c.GetCharacter(ctx, w.character.ID)
		require.NoError(t, err)
		_, err = c.GetPublishedPromptVersion(ctx, content.PromptConversation)
		require.NoError(t, err)
		_, err = c.GetPublishedScenarioVersion(ctx, w.scenario.ID)
		require.NoError(t, err)
		_, err = c.GetTopic(ctx, w.topic.ID)
		require.NoError(t, err)
		_, err = c.GetScenario(ctx, w.scenario.ID)
		require.NoError(t, err)
	}
	require.Equal(t, 1, w.repo.Calls("GetCharacter"), "two reads inside 30 s hit the repository once")
	require.Equal(t, 1, w.repo.Calls("GetPublishedPromptVersion"))
	require.Equal(t, 1, w.repo.Calls("GetPublishedScenarioVersion"))

	clk.Advance(29 * time.Second)
	_, _ = c.GetCharacter(ctx, w.character.ID)
	require.Equal(t, 1, w.repo.Calls("GetCharacter"))
	clk.Advance(2 * time.Second) // 31 s since the first read
	_, _ = c.GetCharacter(ctx, w.character.ID)
	require.Equal(t, 2, w.repo.Calls("GetCharacter"), "the entry expired")
}

func TestCacheByIDDoesNotExpire(t *testing.T) {
	w := newWorld()
	clk := clock.NewFake(t0)
	c := content.NewCachedRepository(w.repo, 30*time.Second, clk)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		_, err := c.GetScenarioVersion(ctx, w.sv.ID)
		require.NoError(t, err)
		_, err = c.GetPromptVersion(ctx, w.prompt.ID)
		require.NoError(t, err)
		_, err = c.GetTopicByID(ctx, w.topic.ID)
		require.NoError(t, err)
		clk.Advance(time.Hour)
	}
	require.Equal(t, 1, w.repo.Calls("GetScenarioVersion"))
	require.Equal(t, 1, w.repo.Calls("GetPromptVersion"))
	require.Equal(t, 1, w.repo.Calls("GetTopicByID"))
}

func TestCacheNotFoundIsCachedForTheTTLOnly(t *testing.T) {
	w := newWorld()
	clk := clock.NewFake(t0)
	c := content.NewCachedRepository(w.repo, 30*time.Second, clk)
	ctx := context.Background()
	missing := uuid.New()
	_, err := c.GetCharacter(ctx, missing)
	require.ErrorIs(t, err, content.ErrNotFound)
	_, err = c.GetCharacter(ctx, missing)
	require.ErrorIs(t, err, content.ErrNotFound)
	require.Equal(t, 1, w.repo.Calls("GetCharacter"))

	_, err = c.GetScenarioVersion(ctx, missing)
	require.ErrorIs(t, err, content.ErrNotFound)
	_, _ = c.GetScenarioVersion(ctx, missing)
	require.Equal(t, 2, w.repo.Calls("GetScenarioVersion"), "a by-id miss is never cached: the row may appear a moment later")
}

func TestCacheDoesNotCacheOtherErrors(t *testing.T) {
	w := newWorld()
	c := content.NewCachedRepository(w.repo, 30*time.Second, clock.NewFake(t0))
	w.repo.FailNext("GetCharacter", errBoom)
	_, err := c.GetCharacter(context.Background(), w.character.ID)
	require.ErrorIs(t, err, errBoom)
	_, err = c.GetCharacter(context.Background(), w.character.ID)
	require.NoError(t, err, "a transient error is not remembered")
}

func TestCacheInvalidate(t *testing.T) {
	w := newWorld()
	c := content.NewCachedRepository(w.repo, time.Hour, clock.NewFake(t0))
	ctx := context.Background()
	_, _ = c.GetCharacter(ctx, w.character.ID)
	_, _ = c.GetScenarioVersion(ctx, w.sv.ID)
	c.(content.Invalidator).Invalidate()
	_, _ = c.GetCharacter(ctx, w.character.ID)
	_, _ = c.GetScenarioVersion(ctx, w.sv.ID)
	require.Equal(t, 2, w.repo.Calls("GetCharacter"))
	require.Equal(t, 2, w.repo.Calls("GetScenarioVersion"))
}

func TestCacheByIDEvictsOldest(t *testing.T) {
	w := newWorld()
	c := content.NewCachedRepository(w.repo, 30*time.Second, clock.NewFake(t0))
	ctx := context.Background()
	first := w.topic.ID
	_, _ = c.GetTopicByID(ctx, first)
	for i := 0; i < 1000; i++ { // 1,000 other topics push the first one out of the 1,000-entry LRU
		topic := content.Topic{ID: uuid.New(), Status: content.StatusActive}
		w.repo.PutTopic(topic)
		_, _ = c.GetTopicByID(ctx, topic.ID)
	}
	before := w.repo.Calls("GetTopicByID")
	_, _ = c.GetTopicByID(ctx, first)
	require.Equal(t, before+1, w.repo.Calls("GetTopicByID"), "the least recently used entry was evicted")
}

func TestCacheListsAreCached(t *testing.T) {
	w := newWorld()
	c := content.NewCachedRepository(w.repo, 30*time.Second, clock.NewFake(t0))
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		topics, err := c.ListTopics(ctx)
		require.NoError(t, err)
		require.Len(t, topics, 1)
		scenarios, err := c.ListScenarios(ctx, w.topic.ID)
		require.NoError(t, err)
		require.Len(t, scenarios, 1)
		chars, err := c.ListCharacters(ctx)
		require.NoError(t, err)
		require.Len(t, chars, 1)
	}
	require.Equal(t, 1, w.repo.Calls("ListTopics"))
	require.Equal(t, 1, w.repo.Calls("ListScenarios"))
	require.Equal(t, 1, w.repo.Calls("ListCharacters"))
}

func TestLoadIgnoresStatus(t *testing.T) {
	w := newWorld()
	w.sv.Status = content.VersionRetired
	w.scenario.Status = content.StatusDisabled
	w.topic.Status = content.StatusDisabled
	w.prompt.Status = content.VersionRetired
	w.put()
	snap, err := content.SessionSnapshot{Character: content.CharacterSnapshot{Code: "HARUKA", Name: "H", Persona: "p"}, Formality: content.FormalityKeigo}.MarshalJSONB()
	require.NoError(t, err)

	cfg, err := content.NewSessionConfigLoader(w.repo).Load(context.Background(), content.SessionRefs{
		TopicID: w.topic.ID, ScenarioVersionID: w.sv.ID, PromptVersionID: w.prompt.ID, ConfigSnapshot: snap})
	require.NoError(t, err, "retired versions and disabled parents still load")
	require.Equal(t, w.sv.ID, cfg.ScenarioVersion.ID)
	require.Equal(t, w.prompt.ID, cfg.PromptVersion.ID)
	require.Equal(t, w.topic.ID, cfg.Topic.ID)
	require.Equal(t, content.FormalityKeigo, cfg.Snapshot.Formality)
}

func TestLoad_UsesSnapshotNotLiveCharacter(t *testing.T) {
	w := newWorld()
	snap := content.SessionSnapshot{Formality: content.FormalityPolite, Character: content.CharacterSnapshot{
		ID: w.character.ID, Code: "HARUKA", Name: "Haruka", Persona: "the persona at creation time",
		ProviderName: "ELEVENLABS", ModelName: "m", VoiceID: "v"}}
	raw, err := snap.MarshalJSONB()
	require.NoError(t, err)

	w.character.Status = content.StatusDisabled
	w.character.Persona = "changed after the session was created"
	w.put()
	cfg, err := content.NewSessionConfigLoader(w.repo).Load(context.Background(), content.SessionRefs{
		TopicID: w.topic.ID, ScenarioVersionID: w.sv.ID, PromptVersionID: w.prompt.ID, ConfigSnapshot: raw})
	require.NoError(t, err)
	require.Equal(t, snap, cfg.Snapshot)
	require.Zero(t, w.repo.Calls("GetCharacter"), "Load never reads the live characters row")
}

func TestLoadErrors(t *testing.T) {
	w := newWorld()
	refs := content.SessionRefs{TopicID: w.topic.ID, ScenarioVersionID: w.sv.ID, PromptVersionID: w.prompt.ID, ConfigSnapshot: []byte(`{"character":{},"formality":"POLITE"}`)}
	l := content.NewSessionConfigLoader(w.repo)

	bad := refs
	bad.ConfigSnapshot = []byte(`{"character":{},"formality":"POLITE","extra":1}`)
	_, err := l.Load(context.Background(), bad)
	require.Equal(t, errs.CodeInternalError, errs.CodeOf(err), "a damaged snapshot is an internal error")
	bad.ConfigSnapshot = nil
	_, err = l.Load(context.Background(), bad)
	require.Equal(t, errs.CodeInternalError, errs.CodeOf(err))

	for name, mutate := range map[string]func(*content.SessionRefs){
		"topic":            func(r *content.SessionRefs) { r.TopicID = uuid.New() },
		"scenario version": func(r *content.SessionRefs) { r.ScenarioVersionID = uuid.New() },
		"prompt version":   func(r *content.SessionRefs) { r.PromptVersionID = uuid.New() },
	} {
		r := refs
		mutate(&r)
		_, err := l.Load(context.Background(), r)
		require.Equal(t, errs.CodeInternalError, errs.CodeOf(err), name)
	}
}

func TestResolve_ObserverCalledPerErrorOnly(t *testing.T) {
	w := newWorld()
	obs := &obsLog{}
	r := content.NewResolver(w.repo, realCfg(), obs)
	_, err := r.Resolve(context.Background(), w.input())
	require.NoError(t, err)
	require.Empty(t, obs.resolve)
	in := w.input()
	in.TopicID = uuid.New()
	_, _ = r.Resolve(context.Background(), in)
	_, _ = r.Resolve(context.Background(), in)
	require.Equal(t, []string{"TOPIC_NOT_AVAILABLE", "TOPIC_NOT_AVAILABLE"}, obs.resolve)
}
