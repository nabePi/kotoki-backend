package testutil_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/testutil"
)

func TestRequireEnvSkips(t *testing.T) {
	t.Setenv("KOTOKI_LIVE_TEST_A", "a")
	t.Setenv("KOTOKI_LIVE_TEST_B", "")
	var skipped bool
	t.Run("missing key skips", func(t *testing.T) {
		t.Cleanup(func() { skipped = t.Skipped() })
		testutil.RequireEnv(t, "KOTOKI_LIVE_TEST_A", "KOTOKI_LIVE_TEST_B")
		t.Fatal("RequireEnv must skip when a key is empty")
	})
	require.True(t, skipped)
}

func TestRequireEnvReturnsValues(t *testing.T) {
	t.Setenv("KOTOKI_LIVE_TEST_A", "a")
	t.Setenv("KOTOKI_LIVE_TEST_B", "b")
	require.Equal(t, map[string]string{"KOTOKI_LIVE_TEST_A": "a", "KOTOKI_LIVE_TEST_B": "b"},
		testutil.RequireEnv(t, "KOTOKI_LIVE_TEST_A", "KOTOKI_LIVE_TEST_B"))
	require.Empty(t, testutil.RequireEnv(t))
}
