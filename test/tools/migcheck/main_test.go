package main

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

func pairs(names ...string) []string {
	var out []string
	for i, n := range names {
		out = append(out, fmt.Sprintf("%06d_%s.up.sql", i+1, n), fmt.Sprintf("%06d_%s.down.sql", i+1, n))
	}
	return out
}

func TestCanonicalAccepted(t *testing.T) {
	require.Len(t, canonical, 18)
	require.NoError(t, checkFiles(pairs(canonical...)))
}

func TestRejectsGap(t *testing.T) {
	files := pairs(canonical[:6]...)
	var kept []string
	for _, f := range files {
		if f[:6] != "000005" {
			kept = append(kept, f)
		}
	}
	require.ErrorContains(t, checkFiles(kept), "000005")
}

func TestRejectsUpWithoutDown(t *testing.T) {
	files := pairs(canonical[:3]...)
	require.ErrorContains(t, checkFiles(files[:len(files)-1]), "down")
}

func TestRejectsNonCanonicalName(t *testing.T) {
	files := pairs("extensions", "users", "foo")
	require.ErrorContains(t, checkFiles(files), "000003_foo")
}

func TestRejectsBadNumberWidth(t *testing.T) {
	require.Error(t, checkFiles([]string{"01_extensions.up.sql", "01_extensions.down.sql"}))
}

func TestModifiedGuard(t *testing.T) {
	require.NoError(t, modifiedGuard(false, []string{"migrations/000002_users.up.sql"}))
	require.Error(t, modifiedGuard(true, []string{"migrations/000002_users.up.sql"}))
	require.NoError(t, modifiedGuard(true, nil))
}
