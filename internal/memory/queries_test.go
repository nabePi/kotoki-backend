package memory_test

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
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

var (
	memoriesRef = regexp.MustCompile(`(?i)\b(?:FROM|JOIN|UPDATE|INTO)\s+memories\b`)
	userPred    = regexp.MustCompile(`user_id = sqlc\.arg\(user_id\)`)
)

func queriesDir() string { return filepath.Join("..", "..", "db", "queries") }

// TestMemoryQueriesAreUserScoped is the static half of the isolation rule of spec 11.10: every
// query that is not Sys-prefixed filters on `user_id = ` at every reference to memories (a
// recursive step, a subquery and a DELETE each need their own), and an INSERT binds the user_id
// column to the caller's argument. An unscoped query fails here.
func TestMemoryQueriesAreUserScoped(t *testing.T) {
	blocks := sqlBlocks(t, filepath.Join(queriesDir(), "memories.sql"))
	require.GreaterOrEqual(t, len(blocks), 17, "the file must hold the whole memory query set")
	for _, want := range []string{
		"ListCoreCandidates", "SearchEpisodic", "FindNearestActive", "GetActiveSlotForUpdate", "InsertMemory",
		"ReinforceMemory", "SupersedeMemory", "HasActiveTombstone", "CountActiveByGroup", "DeleteLowestInGroup",
		"ListMemories", "GetActiveForUpdate", "DeleteSupersedeChain", "DeleteMemoryByID", "UpsertTombstone",
		"DeleteAllMemories", "ListExistingForPrompt", "LockMemoryUser",
	} {
		require.Contains(t, blocks, want)
	}
	for name, body := range blocks {
		require.False(t, strings.HasPrefix(name, "Sys"), "%s: memories.sql holds no cross-user query", name)
		if name == "LockMemoryUser" { // keyed by the caller's user_id, reads and writes no row
			require.Contains(t, body, "sqlc.arg(user_id)")
			require.Zero(t, len(memoriesRef.FindAllString(body, -1)), "%s must not touch the table", name)
			continue
		}
		if strings.Contains(body, "INSERT INTO") {
			require.Regexp(t, `VALUES \(sqlc\.arg\(id\), sqlc\.arg\(user_id\)`, body, "%s: INSERT must bind user_id from the caller", name)
			continue
		}
		refs := len(memoriesRef.FindAllString(body, -1))
		require.Positive(t, refs, "%s: expected a reference to memories", name)
		require.GreaterOrEqual(t, len(userPred.FindAllString(body, -1)), refs,
			"%s: every reference to memories (%d) needs its own `user_id = sqlc.arg(user_id)`", name, refs)
		require.Contains(t, body, "user_id = ", name)
	}
}

// TestMemoryVectorQueriesFilterUserInsideTheCTE is spec 11.10 rule 3: similarity is never a security
// boundary, so the user filter sits inside the MATERIALIZED candidate CTE, before any ranking.
func TestMemoryVectorQueriesFilterUserInsideTheCTE(t *testing.T) {
	blocks := sqlBlocks(t, filepath.Join(queriesDir(), "memories.sql"))
	for _, name := range []string{"SearchEpisodic", "FindNearestActive"} {
		body := blocks[name]
		start := strings.Index(body, "WITH cand AS MATERIALIZED (")
		require.GreaterOrEqual(t, start, 0, "%s: candidates are a MATERIALIZED CTE", name)
		end := strings.Index(body[start:], "\n)\n")
		require.Greater(t, end, 0, name)
		cte := body[start : start+end]
		require.Regexp(t, userPred, cte, "%s: user_id filters inside the CTE", name)
		require.NotContains(t, body[start+end:], "FROM memories", "%s: nothing ranks over the table outside the CTE", name)
		require.NotContains(t, cte, "<=>", "%s: no ranking inside the CTE", name)
	}
}

// TestOnlyExplicitQueriesTouchMemoriesAcrossFiles lists every other query file's access to the
// memories table: the Sys cleanup (cross-user by design, never reached from a handler or the
// runtime) and the account purge, which carries its own user_id.
func TestOnlyExplicitQueriesTouchMemoriesAcrossFiles(t *testing.T) {
	files, err := filepath.Glob(filepath.Join(queriesDir(), "*.sql"))
	require.NoError(t, err)
	var touching []string
	for _, f := range files {
		if filepath.Base(f) == "memories.sql" {
			continue
		}
		for name, body := range sqlBlocks(t, f) {
			if !memoriesRef.MatchString(body) {
				continue
			}
			touching = append(touching, filepath.Base(f)+":"+name)
			if strings.HasPrefix(name, "Sys") {
				continue
			}
			require.Contains(t, body, "user_id = ", "%s must be scoped to a user", name)
		}
	}
	sort.Strings(touching)
	require.Equal(t, []string{"purge.sql:PurgeDeleteMemories", "retention.sql:SysDeleteExpiredMemories"}, touching,
		"a new query on memories outside memories.sql must be reviewed for isolation and listed here")
}
