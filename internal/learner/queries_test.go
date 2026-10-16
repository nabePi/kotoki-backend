package learner_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

var nameLine = regexp.MustCompile(`(?m)^-- name: (\w+) :\w+\s*$`)

// sqlBlocks splits a sqlc query file into its named blocks (name -> text up to the next name).
func sqlBlocks(t *testing.T, path string) map[string]string {
	t.Helper()
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	src := string(raw)
	locs := nameLine.FindAllStringSubmatchIndex(src, -1)
	require.NotEmpty(t, locs, "no -- name: blocks in %s", path)
	out := make(map[string]string, len(locs))
	for i, loc := range locs {
		end := len(src)
		if i+1 < len(locs) {
			end = locs[i+1][0]
		}
		out[src[loc[2]:loc[3]]] = src[loc[1]:end]
	}
	return out
}

// TestLearnerQueriesAreUserScoped is the static half of the isolation rule of spec 12.10: every
// query that is not Sys-prefixed (the advisory lock aside) filters on `user_id = `. An INSERT has no predicate to filter
// with, so it must instead bind the user_id column to the caller's argument.
func TestLearnerQueriesAreUserScoped(t *testing.T) {
	blocks := sqlBlocks(t, filepath.Join("..", "..", "db", "queries", "learner.sql"))
	require.GreaterOrEqual(t, len(blocks), 20, "the file must hold the whole learner query set")
	for name, body := range blocks {
		if strings.HasPrefix(name, "Sys") {
			continue
		}
		if name == "LockLearnerUser" { // keyed by the caller's user_id, reads and writes no row
			require.Contains(t, body, "sqlc.arg(user_id)")
			continue
		}
		if strings.Contains(body, "INSERT INTO") {
			require.Contains(t, body, "sqlc.arg(user_id)", "%s: INSERT must bind user_id from the caller", name)
			continue
		}
		require.Contains(t, body, "user_id = ", "%s: query is not scoped by user_id", name)
	}
}

func TestLearnerSysQueriesAreFew(t *testing.T) {
	blocks := sqlBlocks(t, filepath.Join("..", "..", "db", "queries", "learner.sql"))
	var sys []string
	for name := range blocks {
		if strings.HasPrefix(name, "Sys") {
			sys = append(sys, name)
		}
	}
	require.ElementsMatch(t, []string{"SysLearnerEvaluationExists"}, sys, "only the existence probe may look across users")
}
