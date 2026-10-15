package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func write(t *testing.T, root, rel, src string) {
	t.Helper()
	p := filepath.Join(root, rel)
	require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
	require.NoError(t, os.WriteFile(p, []byte(src), 0o644))
}

const src = "package x\n\nvar m = \"gpt-4.1\"\n"

func TestFindsModelLiteral(t *testing.T) {
	root := t.TempDir()
	write(t, root, "internal/provider/x.go", src)
	got, err := scan([]string{filepath.Join(root, "internal")})
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Contains(t, got[0], "x.go:3")
}

func TestIgnoresTestsConfigAndTestdata(t *testing.T) {
	root := t.TempDir()
	write(t, root, "internal/provider/x_test.go", src)
	write(t, root, "internal/platform/config/c.go", src)
	write(t, root, "internal/testdata/t.go", src)
	write(t, root, "internal/provider/ok.go", "package x\n\nvar m = \"hello\"\n")
	got, err := scan([]string{filepath.Join(root, "internal")})
	require.NoError(t, err)
	require.Empty(t, got)
}

func TestRunExitCodes(t *testing.T) {
	root := t.TempDir()
	write(t, root, "internal/a.go", src)
	var out, errb bytes.Buffer
	require.Equal(t, 1, run([]string{filepath.Join(root, "internal")}, &out, &errb))
	require.True(t, strings.Contains(out.String()+errb.String(), "a.go:3"))
	clean := t.TempDir()
	write(t, clean, "internal/a.go", "package x\n")
	require.Equal(t, 0, run([]string{filepath.Join(clean, "internal")}, &out, &errb))
}
