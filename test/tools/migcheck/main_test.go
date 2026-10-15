package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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

func gitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.CommandContext(context.Background(), "git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@example.test"}, args...)...)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
}

// Deleting or renaming an applied migration is as destructive as editing it.
func TestChangedMigrationsSeesDeleteAndRename(t *testing.T) {
	dir := t.TempDir()
	gitIn(t, dir, "init", "-q", "-b", "main")
	mig := filepath.Join(dir, "migrations")
	require.NoError(t, os.Mkdir(mig, 0o750))
	for _, f := range []string{"000001_a.up.sql", "000002_b.up.sql", "000003_c.up.sql"} {
		require.NoError(t, os.WriteFile(filepath.Join(mig, f), []byte("select 1;\n"), 0o600))
	}
	gitIn(t, dir, "add", ".")
	gitIn(t, dir, "commit", "-q", "-m", "base")

	require.NoError(t, os.Remove(filepath.Join(mig, "000001_a.up.sql")))
	require.NoError(t, os.Rename(filepath.Join(mig, "000002_b.up.sql"), filepath.Join(mig, "000002_renamed.up.sql")))
	require.NoError(t, os.WriteFile(filepath.Join(mig, "000003_c.up.sql"), []byte("select 2;\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(mig, "000004_d.up.sql"), []byte("select 1;\n"), 0o600))
	gitIn(t, dir, "add", "-A")
	gitIn(t, dir, "commit", "-q", "-m", "change")

	got, err := changedMigrations(dir, "HEAD~1", "migrations")
	require.NoError(t, err)
	require.ElementsMatch(t, []string{
		"migrations/000001_a.up.sql", "migrations/000002_b.up.sql", "migrations/000003_c.up.sql",
	}, got, "deleted, renamed-away and edited files count; the new files do not")
}
