package arch

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/testutil"
)

// Supply-chain configuration (spec 13.8.5, plan M10 Task 13): Renovate, gitleaks and the sec-check
// target. None of the tools is needed to run these tests; they check the files' shape and, for the
// gitleaks allowlist, that it cannot hide a real secret.

func readRoot(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(testutil.ModuleRoot(t), rel))
	require.NoError(t, err, rel)
	return string(b)
}

func TestRenovateConfig(t *testing.T) {
	var cfg struct {
		Schema          string   `json:"$schema"`
		Extends         []string `json:"extends"`
		Schedule        []string `json:"schedule"`
		Timezone        string   `json:"timezone"`
		EnabledManagers []string `json:"enabledManagers"`
		PinDigests      *bool    `json:"pinDigests"`
		PackageRules    []struct {
			MatchManagers []string `json:"matchManagers"`
			PinDigests    *bool    `json:"pinDigests"`
		} `json:"packageRules"`
	}
	dec := json.NewDecoder(strings.NewReader(readRoot(t, "renovate.json")))
	require.NoError(t, dec.Decode(&cfg))

	require.Equal(t, "https://docs.renovatebot.com/renovate-schema.json", cfg.Schema)
	require.NotEmpty(t, cfg.Extends)
	require.Contains(t, cfg.Extends, "config:recommended")

	// weekly: every window names one weekday (spec: "Renovate mingguan")
	require.NotEmpty(t, cfg.Schedule)
	weekday := regexp.MustCompile(`\bon (monday|tuesday|wednesday|thursday|friday|saturday|sunday)$`)
	for _, s := range cfg.Schedule {
		require.Regexp(t, weekday, s, "schedule %q must be one weekday per week", s)
	}
	require.NotEmpty(t, cfg.Timezone)

	// the three ecosystems of the spec, and nothing the project does not use
	require.ElementsMatch(t, []string{"gomod", "dockerfile", "github-actions"}, cfg.EnabledManagers)

	require.NotNil(t, cfg.PinDigests)
	require.True(t, *cfg.PinDigests, "pinDigests: images and actions are pinned to digests/commits")
	for _, r := range cfg.PackageRules {
		if r.PinDigests != nil {
			require.True(t, *r.PinDigests, "no package rule may switch digest pinning off: %v", r.MatchManagers)
		}
	}
}

// allowlist is one [[allowlists]] table of .gitleaks.toml.
type allowlist struct {
	description string
	condition   string
	paths       []string
	regexes     []string
	targetRules []string
	keys        []string
	perRule     bool   // [[rules.allowlists]] (applies to one rule) rather than a global [[allowlists]]
	rule        string // id of the [[rules]] table it belongs to
}

// ruleTable is one [[rules]] table.
type ruleTable struct {
	id   string
	keys []string
}

var (
	tomlKey   = regexp.MustCompile(`^([A-Za-z]+)\s*=`)
	tomlItem  = regexp.MustCompile(`^\s*'''(.*)'''\s*,?\s*$`)
	tomlQuote = regexp.MustCompile(`^\s*"([^"]*)"\s*,?\s*$`)
)

// parseGitleaks reads the file line by line: it knows only the shapes this repository writes (one
// ”'literal”' or "string" per line inside arrays that close with a lone "]"), and fails on
// anything else, so an exotic key cannot slip past the checks below.
func parseGitleaks(t *testing.T, text string) (lists []allowlist, rules []ruleTable, extendDefault bool, tables []string) {
	t.Helper()
	var cur *allowlist
	var curRule *ruleTable
	section := ""
	lines := strings.Split(text, "\n")
	for i := 0; i < len(lines); i++ {
		line := strings.TrimSpace(lines[i])
		switch {
		case line == "" || strings.HasPrefix(line, "#"):
			continue
		case strings.HasPrefix(line, "["):
			section = line
			tables = append(tables, line)
			cur = nil
			switch line {
			case "[[allowlists]]":
				lists = append(lists, allowlist{})
				cur = &lists[len(lists)-1]
			case "[[rules]]":
				rules = append(rules, ruleTable{})
				curRule = &rules[len(rules)-1]
			case "[[rules.allowlists]]":
				require.NotNil(t, curRule, "[[rules.allowlists]] without a [[rules]]")
				lists = append(lists, allowlist{perRule: true, rule: curRule.id})
				cur = &lists[len(lists)-1]
			}
			continue
		}
		m := tomlKey.FindStringSubmatch(line)
		require.NotNil(t, m, "line %d: unsupported TOML %q", i+1, line)
		key, rest := m[1], strings.TrimSpace(line[len(m[0]):])
		if section == "[extend]" && key == "useDefault" {
			extendDefault = rest == "true"
			continue
		}
		if section == "[[rules]]" && curRule != nil {
			curRule.keys = append(curRule.keys, key)
			if key == "id" {
				curRule.id = strings.Trim(rest, `"`)
			}
			continue
		}
		if cur == nil {
			continue
		}
		cur.keys = append(cur.keys, key)
		var arr []string
		if rest == "[" {
			for i++; i < len(lines) && strings.TrimSpace(lines[i]) != "]"; i++ {
				l := lines[i]
				if strings.HasPrefix(strings.TrimSpace(l), "#") || strings.TrimSpace(l) == "" {
					continue
				}
				if im := tomlItem.FindStringSubmatch(l); im != nil {
					arr = append(arr, im[1])
				} else if qm := tomlQuote.FindStringSubmatch(l); qm != nil {
					arr = append(arr, qm[1])
				} else {
					t.Fatalf("line %d: unsupported array item %q", i+1, l)
				}
			}
		}
		switch key {
		case "description":
			cur.description = strings.Trim(rest, `"`)
		case "condition":
			cur.condition = strings.Trim(rest, `"`)
		case "paths":
			cur.paths = arr
		case "regexes":
			cur.regexes = arr
		case "targetRules":
			cur.targetRules = arr
		}
	}
	return lists, rules, extendDefault, tables
}

func TestGitleaksConfigAllowsOnlyExamples(t *testing.T) {
	lists, rules, extendDefault, tables := parseGitleaks(t, readRoot(t, ".gitleaks.toml"))
	require.True(t, extendDefault, "the default rules stay on: [extend] useDefault = true")
	require.NotEmpty(t, lists)
	for _, tb := range tables {
		// no legacy global [allowlist] and no custom rule that could be turned into a blanket exemption
		require.Contains(t, []string{"[extend]", "[[allowlists]]", "[[rules]]", "[[rules.allowlists]]"}, tb, "table %s", tb)
	}
	// [[rules]] only names a default rule to attach an allowlist to: redefining its regex, entropy,
	// keywords or path would weaken the detection itself.
	defaults := map[string]bool{"generic-api-key": true, "stripe-access-token": true, "curl-auth-user": true}
	for _, r := range rules {
		require.Equal(t, []string{"id"}, r.keys, "rule %q may only carry its id", r.id)
		require.True(t, defaults[r.id], "rule %q: only the rules that fire on the repository's dummy values may be amended", r.id)
	}

	// Only these two locations may be exempt without looking at the value: the documented example
	// env file and test fixtures.
	unconditional := map[string]bool{`(^|/)\.env\.example$`: true, `(^|/)testdata/`: true}
	// Anywhere else a path must come with a regex on the secret itself (condition AND), so a real
	// secret committed to the same file is still reported.
	scoped := map[string]bool{
		`^deploy/k8s/overlays/dev/kustomization\.yaml$`: true,
		`^internal/provider/httpx/httpx_test\.go$`:      true,
		`^docs/superpowers/plans/[^/]+\.md$`:            true,
	}

	var pathRes, valueRes []*regexp.Regexp
	seenUnconditional := map[string]bool{}
	for _, l := range lists {
		require.NotEmpty(t, l.description, "every allowlist says why it exists")
		for _, k := range l.keys {
			require.Contains(t, []string{"description", "condition", "paths", "regexes", "targetRules", "regexTarget"}, k,
				"allowlist %q: key %s (commits, stopwords and the like would hide secrets by identity)", l.description, k)
		}
		require.NotEmpty(t, l.paths, "allowlist %q must name paths", l.description)
		if len(l.regexes) == 0 {
			require.False(t, l.perRule, "allowlist %q: a per-rule allowlist needs a value regex", l.description)
			for _, p := range l.paths {
				require.True(t, unconditional[p], "allowlist %q: path %q may not be exempt without a value regex", l.description, p)
				seenUnconditional[p] = true
			}
		} else {
			require.True(t, l.perRule, "allowlist %q: a global allowlist skips whole files, so it cannot be scoped by value; use [[rules.allowlists]]", l.description)
			require.Equal(t, "AND", l.condition, "allowlist %q: path AND value", l.description)
			for _, p := range l.paths {
				require.True(t, scoped[p], "allowlist %q: unexpected path %q", l.description, p)
			}
		}
		for _, p := range l.paths {
			pathRes = append(pathRes, regexp.MustCompile(p))
		}
		for _, r := range l.regexes {
			re := regexp.MustCompile(r)
			valueRes = append(valueRes, re)
			// a value regex has to be specific: an unanchored short or universal pattern is a skeleton key
			require.GreaterOrEqual(t, len(r), 8, "allowlist %q: regex %q is too loose", l.description, r)
			require.False(t, re.MatchString("xK9p2LmQ8vRt4NzW7bYc5Hd3Fg6Js1Aa"), "regex %q matches a random-looking token", r)
		}
	}
	require.Equal(t, unconditional, seenUnconditional, "both example locations are listed")

	// the path patterns behave: example files in, everything that matters out
	matchesAny := func(s string) bool {
		for _, re := range pathRes {
			if re.MatchString(s) {
				return true
			}
		}
		return false
	}
	for _, p := range []string{".env.example", "testdata/api/rest/auth/register_201.json", "internal/auth/testdata/x.json"} {
		require.True(t, matchesAny(p), p)
	}
	for _, p := range []string{
		".env", ".env.local", ".env.production", "internal/auth/jwt.go", "internal/provider/httpx/httpx.go", "cmd/api/main.go",
		"deploy/k8s/overlays/prod/kustomization.yaml", "deploy/k8s/overlays/staging/kustomization.yaml", "deploy/k8s/base/api/deployment.yaml",
		"deploy/docker/api.Dockerfile", ".github/workflows/ci.yml", ".github/workflows/deploy-prod.yml", "docker-compose.yml", "seed/env/local/provider.env",
		"docs/security/asvs-l2.md", "Makefile", "go.mod", "testdata.txt", "mytestdata/x",
	} {
		require.False(t, matchesAny(p), "%s must not be exempt", p)
	}

	// no value pattern matches a credential-shaped string
	// (assembled at run time: the literals themselves must not look like secrets to gitleaks)
	for _, secret := range []string{
		"AK" + "IAZ7Q3R5T9W2X4Y6B8", "sk-" + "proj-7Hq2LmZ9xV4nB8tR1wC5yD3fG6jK0aPeS", "gh" + "p_aB3dE5gH7jK9mN1pQ3sT5vX7zA9cF1hJ3kL5",
		"xo" + "xb-1234567890-1234567890123-aBcDeFgHiJkLmNoPqRsTuVwX", "ey" + "JhbGciOiJIUzI1NiJ9.eyJzdWIiOiJ4In0.kQ8vN2mP4xR6tY9uI1oL3aS5dF7gH0jZ",
		"postgres://kotoki:" + "Zq8Lm2Vx9Rt4Nw7B@db.internal.kotoki.id:5432/kotoki",
	} {
		for _, re := range valueRes {
			require.False(t, re.MatchString(secret), "regex %q would exempt %q", re, secret)
		}
	}
}

func TestSecCheckTarget(t *testing.T) {
	mk := readRoot(t, "Makefile")
	_, rest, ok := strings.Cut(mk, "\nsec-check:")
	require.True(t, ok, "Makefile has a sec-check target")
	recipe, _, _ := strings.Cut(rest, "\n\n")
	require.Contains(t, recipe, "$(MAKE) vuln")
	require.Contains(t, recipe, "gitleaks detect --config .gitleaks.toml")
	require.Contains(t, recipe, "trivy fs --severity CRITICAL,HIGH --ignore-unfixed")
	require.Regexp(t, `(?m)^\.PHONY:.*\bsec-check\b`, strings.ReplaceAll(mk, "\\\n", " "))
}

// TestAsvsChecklistEvidenceExists keeps docs/security/asvs-l2.md from rotting: every test name and
// every file path quoted in backticks must exist, and an AUTO row must quote at least one.
func TestAsvsChecklistEvidenceExists(t *testing.T) {
	root := testutil.ModuleRoot(t)
	doc := readRoot(t, "docs/security/asvs-l2.md")

	// every test function declared anywhere in the repository
	declared := map[string]bool{}
	decl := regexp.MustCompile(`(?m)^func (Test[A-Za-z0-9_]+)\(`)
	require.NoError(t, filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if n := d.Name(); n == ".git" || n == "bin" || n == "out" || n == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(path, "_test.go") {
			b, rerr := os.ReadFile(path)
			if rerr != nil {
				return rerr
			}
			for _, m := range decl.FindAllSubmatch(b, -1) {
				declared[string(m[1])] = true
			}
		}
		return nil
	}))

	span := regexp.MustCompile("`([^`]+)`")
	validStatus := map[string]bool{"AUTO": true, "MANUAL": true, "N/A": true}
	rows := 0
	for _, line := range strings.Split(doc, "\n") {
		if !strings.HasPrefix(line, "| V") {
			continue
		}
		cells := strings.Split(strings.Trim(line, "| "), " | ")
		require.Len(t, cells, 4, "row %q: Bab ASVS | Kontrol | Status | Bukti", line)
		rows++
		status, evidence := strings.TrimSpace(cells[2]), cells[3]
		require.True(t, validStatus[status], "row %q: status %q", line, status)
		quoted := 0
		for _, m := range span.FindAllStringSubmatch(evidence, -1) {
			ref := m[1]
			switch {
			case regexp.MustCompile(`^Test[A-Za-z0-9_]+$`).MatchString(ref):
				quoted++
				require.True(t, declared[ref], "row %q: test %s does not exist", line, ref)
			case !strings.ContainsAny(ref, " ") && (strings.Contains(ref, "/") || strings.Contains(ref, ".")):
				quoted++
				_, err := os.Stat(filepath.Join(root, ref))
				require.NoError(t, err, "row %q: %s does not exist", line, ref)
			}
		}
		if status == "AUTO" {
			require.Positive(t, quoted, "row %q: an AUTO control must point at a test or a file", line)
		}
		if status == "MANUAL" {
			require.Regexp(t, `MG-\d`, evidence, "row %q: a MANUAL control belongs to a gate of Task 24", line)
		}
	}
	require.GreaterOrEqual(t, rows, 40)
	for _, ch := range []string{"V1", "V2", "V3", "V4", "V5", "V6", "V7", "V8", "V9", "V10", "V11", "V12", "V13", "V14"} {
		require.Regexp(t, `(?m)^\| `+ch+`\.\d`, doc, "chapter %s has rows", ch)
	}
}

// TestCIGitleaksStep: the gitleaks action bundles gitleaks 8.24.3, which does not read the
// [[allowlists]] tables of .gitleaks.toml (it reports every allowlisted dummy value as a leak), so
// the workflow has to pin a newer gitleaks and name the config.
func TestCIGitleaksStep(t *testing.T) {
	ci := readCI(t)
	_, step, ok := strings.Cut(ci, "uses: gitleaks/gitleaks-action@")
	require.True(t, ok, "ci.yml runs gitleaks")
	step, _, _ = strings.Cut(step, "\n  unit:")
	m := regexp.MustCompile(`GITLEAKS_VERSION: "?(\d+)\.(\d+)\.(\d+)"?`).FindStringSubmatch(step)
	require.NotNil(t, m, "GITLEAKS_VERSION is pinned")
	major, minor := atoi(t, m[1]), atoi(t, m[2])
	require.True(t, major > 8 || (major == 8 && minor >= 25), "gitleaks >= 8.25 reads [[allowlists]], got %s.%s", m[1], m[2])
	require.Contains(t, step, "GITLEAKS_CONFIG: .gitleaks.toml")
}

func atoi(t *testing.T, s string) int {
	t.Helper()
	n, err := strconv.Atoi(s)
	require.NoError(t, err)
	return n
}
