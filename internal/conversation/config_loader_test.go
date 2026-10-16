package conversation

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/content"
	"github.com/nabePi/kotoki-backend/internal/platform/errs"
	"github.com/nabePi/kotoki-backend/internal/session"
)

// fakeContent stands in for content.SessionConfigLoader and the prompt repository. Its methods
// that must never run (live/PUBLISHED lookups) panic.
type fakeContent struct {
	loads, promptGets int
	lastRefs          content.SessionRefs
}

func (f *fakeContent) Load(_ context.Context, refs content.SessionRefs) (content.SessionConfig, error) {
	f.loads++
	f.lastRefs = refs
	snap, err := content.ParseSnapshot(refs.ConfigSnapshot)
	if err != nil {
		return content.SessionConfig{}, err
	}
	return content.SessionConfig{
		Topic:           content.Topic{ID: refs.TopicID},
		ScenarioVersion: content.ScenarioVersion{ID: refs.ScenarioVersionID},
		Snapshot:        snap,
		PromptVersion:   content.PromptVersion{ID: refs.PromptVersionID},
	}, nil
}

func (f *fakeContent) GetPromptVersion(_ context.Context, id uuid.UUID) (content.PromptVersion, error) {
	f.promptGets++
	return content.PromptVersion{ID: id, Key: content.PromptKey("OPENING")}, nil
}

func (f *fakeContent) GetPublishedPromptVersion(context.Context, string) (content.PromptVersion, error) {
	panic("config loader must never query PUBLISHED prompt versions")
}

func snapshotJSON(t *testing.T, name string) json.RawMessage {
	t.Helper()
	b, err := content.SessionSnapshot{
		Character: content.CharacterSnapshot{ID: uuid.New(), Code: "c", Name: name, Persona: "p", ProviderName: "FAKE", ModelName: "m", VoiceID: "v",
			Voice: content.Voice{Gender: "FEMALE", Accent: "STANDARD", SpeakingRate: 1}},
		Formality: content.FormalityPolite,
	}.MarshalJSONB()
	require.NoError(t, err)
	return b
}

func runtimeRec(t *testing.T) session.RuntimeRecord {
	t.Helper()
	return session.RuntimeRecord{
		ID: uuid.New(), TopicID: uuid.New(), ScenarioVersionID: uuid.New(), PromptVersionID: uuid.New(),
		ConfigSnapshot: snapshotJSON(t, "Yuki"),
	}
}

func TestConfigLoaderCachesByID(t *testing.T) {
	f := &fakeContent{}
	l := NewConfigLoader(f, f, 8)
	rec := runtimeRec(t)

	first, err := l.LoadSnapshot(context.Background(), rec)
	require.NoError(t, err)
	require.Equal(t, 1, f.loads)
	require.Nil(t, first.Opening)
	require.Zero(t, f.promptGets, "no opening id means no prompt query")

	second, err := l.LoadSnapshot(context.Background(), rec)
	require.NoError(t, err)
	require.Equal(t, 1, f.loads, "second call must not touch SessionConfigLoader")
	require.Equal(t, first, second)
	require.Equal(t, rec.PromptVersionID, second.PromptVersion.ID)
	require.Equal(t, rec.ScenarioVersionID, second.ScenarioVersion.ID)
	require.Equal(t, rec.TopicID, second.Topic.ID)

	// the opening prompt is loaded by id from the record and cached
	oid := uuid.New()
	rec.OpeningPromptVersionID = &oid
	for i := 0; i < 2; i++ {
		got, err := l.LoadSnapshot(context.Background(), rec)
		require.NoError(t, err)
		require.NotNil(t, got.Opening)
		require.Equal(t, oid, got.Opening.ID)
	}
	require.Equal(t, 1, f.promptGets)
	require.Equal(t, 1, f.loads)
}

func TestConfigLoaderNeverQueriesPublished(t *testing.T) {
	f := &fakeContent{}
	l := NewConfigLoader(f, f, 8) // any call to GetPublishedPromptVersion panics
	rec := runtimeRec(t)
	oid := uuid.New()
	rec.OpeningPromptVersionID = &oid
	require.NotPanics(t, func() {
		_, err := l.LoadSnapshot(context.Background(), rec)
		require.NoError(t, err)
	})
}

func TestConfigLoaderUsesSnapshot(t *testing.T) {
	f := &fakeContent{}
	l := NewConfigLoader(f, f, 8)
	rec := runtimeRec(t)

	got, err := l.LoadSnapshot(context.Background(), rec)
	require.NoError(t, err)
	require.JSONEq(t, string(rec.ConfigSnapshot), string(f.lastRefs.ConfigSnapshot))
	require.Equal(t, "Yuki", got.Snapshot.Character.Name)
	require.Equal(t, "FAKE", got.Snapshot.Character.ProviderName)
	require.Equal(t, content.FormalityPolite, got.Snapshot.Formality)

	// same versions, another session with a different character snapshot: versions come from the
	// cache but the character is decoded per session (a character disabled after creation, or a
	// changed one, never leaks across sessions).
	other := rec
	other.ID = uuid.New()
	other.ConfigSnapshot = snapshotJSON(t, "Haru")
	got2, err := l.LoadSnapshot(context.Background(), other)
	require.NoError(t, err)
	require.Equal(t, 1, f.loads)
	require.Equal(t, "Haru", got2.Snapshot.Character.Name)
	require.Equal(t, "Yuki", got.Snapshot.Character.Name)
}

func TestConfigLoaderBrokenSnapshot(t *testing.T) {
	for name, raw := range map[string]json.RawMessage{
		"garbage":       json.RawMessage(`{not json`),
		"empty":         nil,
		"unknown field": json.RawMessage(`{"character":{},"formality":"POLITE","extra":1}`),
	} {
		t.Run(name, func(t *testing.T) {
			f := &fakeContent{}
			l := NewConfigLoader(f, f, 8)
			rec := runtimeRec(t)
			rec.ConfigSnapshot = raw
			_, err := l.LoadSnapshot(context.Background(), rec)
			require.Error(t, err)
			require.Equal(t, errs.CodeContentMisconfigured, errs.CodeOf(err))
			require.Zero(t, f.loads)
		})
	}
}

func TestConfigLoaderLRUEvicts(t *testing.T) {
	f := &fakeContent{}
	l := NewConfigLoader(f, f, 1)
	a, b := runtimeRec(t), runtimeRec(t)
	for _, r := range []session.RuntimeRecord{a, b, a} {
		_, err := l.LoadSnapshot(context.Background(), r)
		require.NoError(t, err)
	}
	require.Equal(t, 3, f.loads, "size 1: b evicts a, so a loads again")
}
