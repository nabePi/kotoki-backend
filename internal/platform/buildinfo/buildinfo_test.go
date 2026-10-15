package buildinfo_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/platform/buildinfo"
)

func TestDefaults(t *testing.T) {
	require.Equal(t, "dev", buildinfo.Version)
	require.Equal(t, "unknown", buildinfo.GitCommit)
	require.Equal(t, "dev (unknown)", buildinfo.String())
}
