package testutil

import (
	"os"
	"path/filepath"
	"testing"
)

// ModuleRoot walks up from the working directory to the directory holding go.mod.
func ModuleRoot(t testing.TB) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("testutil: getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("testutil: go.mod not found above working directory")
		}
		dir = parent
	}
}

// TestdataPath returns <module root>/testdata/<rel>, failing if it does not exist.
func TestdataPath(t testing.TB, rel string) string {
	t.Helper()
	p := filepath.Join(ModuleRoot(t), "testdata", filepath.FromSlash(rel))
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("testutil: testdata %q: %v", rel, err)
	}
	return p
}
