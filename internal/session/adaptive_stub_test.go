package session_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/content"
	"github.com/nabePi/kotoki-backend/internal/session"
)

func adaptiveFor(t *testing.T, f content.Formality) map[string]any {
	t.Helper()
	raw, err := session.NeutralAdaptiveProvider{}.Adaptive(context.Background(), session.AdaptiveRequest{
		Config: content.ResolvedConfig{ConfigSnapshot: content.SessionSnapshot{Formality: f}}})
	require.NoError(t, err)
	var m map[string]any
	require.NoError(t, json.Unmarshal(raw, &m), "a JSON object")
	return m
}

func TestNeutralAdaptiveProvider(t *testing.T) {
	keigo, polite := adaptiveFor(t, content.FormalityKeigo), adaptiveFor(t, content.FormalityPolite)
	require.EqualValues(t, 1, keigo["algorithm_version"])
	require.EqualValues(t, 0, keigo["in_session_adjustments"])
	require.Equal(t, "KEIGO", keigo["formality"])
	require.Equal(t, "POLITE", polite["formality"])
	require.Equal(t, "NONE", keigo["dialect_exposure"])
	require.Equal(t, []any{}, keigo["focus_skills"])
	require.InDelta(t, 0.95, keigo["tts_speaking_rate"], 1e-9)

	delete(keigo, "formality")
	delete(polite, "formality")
	require.Equal(t, keigo, polite, "nothing but formality depends on the input")
	require.Len(t, keigo, 19)
	require.Equal(t, adaptiveFor(t, content.FormalityKeigo)["scenario_difficulty"], adaptiveFor(t, content.FormalityCasual)["scenario_difficulty"])

	a, _ := session.NeutralAdaptiveProvider{}.Adaptive(context.Background(), session.AdaptiveRequest{})
	b, _ := session.NeutralAdaptiveProvider{}.Adaptive(context.Background(), session.AdaptiveRequest{})
	require.Equal(t, string(a), string(b), "two calls give the same answer")
}
