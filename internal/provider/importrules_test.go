package provider_test

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

const modPath = "github.com/nabePi/kotoki-backend/"

// importsOf returns the imports of every non-test .go file directly in dir.
func importsOf(t *testing.T, dir string) map[string][]string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "*.go"))
	require.NoError(t, err)
	out := map[string][]string{}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		af, err := parser.ParseFile(token.NewFileSet(), f, nil, parser.ImportsOnly)
		require.NoError(t, err)
		for _, im := range af.Imports {
			p, err := strconv.Unquote(im.Path.Value)
			require.NoError(t, err)
			out[f] = append(out[f], p)
		}
	}
	return out
}

func isStdlib(p string) bool { return !strings.Contains(strings.SplitN(p, "/", 2)[0], ".") }

func TestImportRulesProvider(t *testing.T) {
	allowedExt := []string{
		"github.com/google/uuid", "golang.org/x/sync/semaphore",
		"github.com/prometheus/client_golang", modPath + "internal/platform/clock",
	}
	for f, imps := range importsOf(t, ".") {
		for _, p := range imps {
			if isStdlib(p) {
				require.False(t, p == "net/http" || strings.HasPrefix(p, "net/http/"), "%s imports %s", f, p)
				continue
			}
			ok := false
			for _, a := range allowedExt {
				ok = ok || p == a || strings.HasPrefix(p, a+"/")
			}
			require.True(t, ok, "%s imports forbidden package %s", f, p)
		}
	}
}

func TestImportRulesFake(t *testing.T) {
	if _, err := os.Stat("fake"); err != nil {
		t.Skip("fake package not created yet")
	}
	for f, imps := range importsOf(t, "fake") {
		for _, p := range imps {
			require.NotContains(t, p, "go-redis", f)
			require.NotContains(t, p, "provider/httpx", f)
		}
	}
}

func TestImportRulesPrompt(t *testing.T) {
	if _, err := os.Stat("../prompt"); err != nil {
		t.Skip("prompt package not created yet")
	}
	banned := []string{"auth", "balance", "session", "content", "conversation", "provider", "evaluation", "memory", "learner", "summary", "admin", "events"}
	for f, imps := range importsOf(t, "../prompt") {
		for _, p := range imps {
			for _, b := range banned {
				require.False(t, p == modPath+"internal/"+b || strings.HasPrefix(p, modPath+"internal/"+b+"/"), "%s imports %s", f, p)
			}
		}
	}
}
