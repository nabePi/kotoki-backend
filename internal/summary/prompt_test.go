package summary_test

import (
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/summary"
)

func TestPromptFixtures(t *testing.T) {
	fx := summary.PromptFixtures()
	require.Len(t, fx, 2)
	lo, ok := fx[0].(summary.SummaryPromptData)
	require.True(t, ok)
	hi, ok := fx[1].(summary.SummaryPromptData)
	require.True(t, ok)
	require.NotEmpty(t, lo.Transcript)
	require.NotContains(t, lo.Transcript, "\n")
	require.Equal(t, 30000, utf8.RuneCountInString(hi.Transcript))
	for _, d := range []summary.SummaryPromptData{lo, hi} {
		require.Equal(t, 300, d.MaxChars)
		require.NotEmpty(t, d.TopicName)
		require.NotEmpty(t, d.ScenarioName)
		require.Positive(t, d.UserTurnCount)
	}
}

func TestDefaultConfig(t *testing.T) {
	require.Equal(t, summary.Config{MaxChars: 300}, summary.DefaultConfig())
}
