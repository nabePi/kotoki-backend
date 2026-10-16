package memory_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/memory"
)

// Characterisation of the M5 types against spec 11.7.1.
func TestMemoryContextJSONKeys(t *testing.T) {
	b, err := json.Marshal(memory.MemoryContext{})
	require.NoError(t, err)
	var m map[string]any
	require.NoError(t, json.Unmarshal(b, &m))
	for _, k := range []string{"session_id", "user_mem_ver", "core", "core_tokens", "episodic", "episodic_tokens", "built_at", "degraded"} {
		require.Contains(t, m, k)
	}
	require.Len(t, m, 8)
}

func TestContextItemJSONKeys(t *testing.T) {
	b, err := json.Marshal(memory.ContextItem{})
	require.NoError(t, err)
	var m map[string]any
	require.NoError(t, json.Unmarshal(b, &m))
	for _, k := range []string{"id", "category", "content", "observed_at", "score", "tokens"} {
		require.Contains(t, m, k)
	}
	require.Len(t, m, 6)
}

func TestCategoryValues(t *testing.T) {
	require.Equal(t, []memory.Category{"PROFILE_FACT", "PREFERENCE", "LEARNING_PATTERN", "EPISODIC"},
		[]memory.Category{memory.CategoryProfileFact, memory.CategoryPreference, memory.CategoryLearningPattern, memory.CategoryEpisodic})
}
