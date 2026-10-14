package prompt_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/prompt"
)

func TestDefaultBudgetTotal(t *testing.T) {
	require.Equal(t, 6450, prompt.DefaultBudget.Total())
	require.Equal(t, 4, prompt.DefaultBudget.MinRecentTurns)
	require.Equal(t, 600, prompt.DefaultBudget.System)
	require.Equal(t, 500, prompt.DefaultBudget.Character)
	require.Equal(t, 400, prompt.DefaultBudget.Scenario)
	require.Equal(t, 700, prompt.DefaultBudget.Learner)
	require.Equal(t, 600, prompt.DefaultBudget.MemoryCore)
	require.Equal(t, 500, prompt.DefaultBudget.MemoryEpisodic)
	require.Equal(t, 500, prompt.DefaultBudget.Summary)
	require.Equal(t, 50, prompt.DefaultBudget.SessionState)
	require.Equal(t, 2000, prompt.DefaultBudget.RecentTurns)
	require.Equal(t, 600, prompt.DefaultBudget.CurrentInput)
}

func TestBudgetTotalExcludesMinRecentTurns(t *testing.T) {
	b := prompt.Budget{System: 1, MinRecentTurns: 99}
	require.Equal(t, 1, b.Total())
}
