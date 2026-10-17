// Command readinesscheck validates docs/production-readiness.md (spec 13.14.1, M10 Task 23).
//
// The document holds a table `| Butir | Status | Bukti |`. Every line of items.txt must appear in it
// exactly once, no other row may exist, the status must be one of DONE, LOCAL-VERIFIED, MANUAL-GATE
// or EXTERNAL, and the evidence must be honest:
//   - DONE and LOCAL-VERIFIED need evidence, and every backticked path in it must exist in the repo
//     (DONE needs at least one path; LOCAL-VERIFIED also names the command that was run);
//   - MANUAL-GATE names the MG-n of M10 Task 24; EXTERNAL needs a non-empty reason.
//
// Usage: go run ./test/tools/readinesscheck [-doc docs/production-readiness.md] [-items test/tools/readinesscheck/items.txt]
package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// repoRoot is the directory evidence paths are resolved against; tests point it at the repo root.
var repoRoot = "."

var validStatus = map[string]bool{"DONE": true, "LOCAL-VERIFIED": true, "MANUAL-GATE": true, "EXTERNAL": true}

var (
	codeSpan = regexp.MustCompile("`([^`]+)`")
	mgRef    = regexp.MustCompile(`\bMG-[1-9]\b`)
	emptyEv  = map[string]bool{"": true, "-": true, "tbd": true, "todo": true, "n/a": true}
)

type row struct {
	item, status, evidence string
	line                   int
}

func parseRows(md string) []row {
	var rows []row
	inTable := false
	for i, ln := range strings.Split(md, "\n") {
		t := strings.TrimSpace(ln)
		if !strings.HasPrefix(t, "|") {
			inTable = inTable && t == ""
			continue
		}
		cells := strings.Split(strings.Trim(t, "|"), "|")
		for j := range cells {
			cells[j] = strings.TrimSpace(cells[j])
		}
		if len(cells) < 3 {
			continue
		}
		if cells[0] == "Butir" {
			inTable = true
			continue
		}
		if !inTable || strings.Trim(cells[0], "-: ") == "" {
			continue
		}
		rows = append(rows, row{
			item: cells[0], status: strings.Trim(cells[1], "` "),
			evidence: strings.Join(cells[2:], "|"), line: i + 1,
		})
	}
	return rows
}

// evidencePaths returns the backticked tokens of ev that look like repo paths (a slash, no spaces,
// no URL, no placeholder). A suffix after ':' or '#' (test name, anchor) is dropped.
func evidencePaths(ev string) []string {
	var out []string
	for _, m := range codeSpan.FindAllStringSubmatch(ev, -1) {
		tok := m[1]
		if strings.ContainsAny(tok, " \t<>*{}$") || strings.Contains(tok, "://") || !strings.Contains(tok, "/") {
			continue
		}
		if i := strings.IndexAny(tok, ":#"); i >= 0 {
			tok = tok[:i]
		}
		tok = strings.TrimPrefix(tok, "./")
		if strings.HasPrefix(tok, "/") || strings.HasPrefix(tok, "..") || tok == "" {
			continue
		}
		out = append(out, tok)
	}
	return out
}

func hasCommand(ev string) bool {
	for _, m := range codeSpan.FindAllStringSubmatch(ev, -1) {
		if strings.ContainsAny(m[1], " \t") {
			return true
		}
	}
	return false
}

// Check validates the readiness markdown against the required items and returns every problem found.
func Check(md string, items []string) []error {
	var errs []error
	add := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	rows := parseRows(md)
	if len(rows) == 0 {
		return []error{fmt.Errorf("no `| Butir | Status | Bukti |` table rows found")}
	}
	want := map[string]bool{}
	for _, it := range items {
		want[it] = true
	}
	count := map[string]int{}
	for _, r := range rows {
		count[r.item]++
	}
	for _, it := range items {
		switch n := count[it]; {
		case n == 0:
			add("item missing: %q", it)
		case n > 1:
			add("item appears more than once (%d): %q", n, it)
		}
	}
	for _, r := range rows {
		if !want[r.item] {
			add("line %d: unknown item %q", r.line, r.item)
			continue
		}
		if !validStatus[r.status] {
			add("line %d: invalid status %q for %q (want DONE, LOCAL-VERIFIED, MANUAL-GATE or EXTERNAL)", r.line, r.status, r.item)
			continue
		}
		if emptyEv[strings.ToLower(strings.TrimSpace(r.evidence))] {
			add("line %d: %s needs evidence (empty): %q", r.line, r.status, r.item)
			continue
		}
		switch r.status {
		case "MANUAL-GATE":
			if !mgRef.MatchString(r.evidence) {
				add("line %d: MANUAL-GATE must name an MG-n of Task 24: %q", r.line, r.item)
			}
		case "DONE", "LOCAL-VERIFIED":
			paths := evidencePaths(r.evidence)
			if r.status == "DONE" && len(paths) == 0 {
				add("line %d: DONE needs at least one backticked repo path as evidence: %q", r.line, r.item)
			}
			if r.status == "LOCAL-VERIFIED" && !hasCommand(r.evidence) {
				add("line %d: LOCAL-VERIFIED must name the command that was run (backticked, with arguments): %q", r.line, r.item)
			}
			for _, p := range paths {
				if _, err := os.Stat(filepath.Join(repoRoot, filepath.FromSlash(p))); err != nil {
					add("line %d: evidence path %q does not exist: %q", r.line, p, r.item)
				}
			}
		}
	}
	return errs
}

// loadItems reads items.txt: one item per line, blank lines and `#` comments ignored.
func loadItems(path string) ([]string, error) {
	f, err := os.Open(path) //nolint:gosec // dev CLI: the path is a flag
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	var out []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if l := strings.TrimSpace(sc.Text()); l != "" && !strings.HasPrefix(l, "#") {
			out = append(out, l)
		}
	}
	return out, sc.Err()
}

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("readinesscheck", flag.ContinueOnError)
	fs.SetOutput(stderr)
	doc := fs.String("doc", "docs/production-readiness.md", "readiness document")
	itemsPath := fs.String("items", "test/tools/readinesscheck/items.txt", "required items")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	items, err := loadItems(*itemsPath)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "readiness: %v\n", err)
		return 2
	}
	b, err := os.ReadFile(*doc)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "readiness: %v\n", err)
		return 2
	}
	if errs := Check(string(b), items); len(errs) > 0 {
		for _, e := range errs {
			_, _ = fmt.Fprintf(stderr, "readiness: %v\n", e)
		}
		_, _ = fmt.Fprintf(stderr, "readiness: %d problem(s)\n", len(errs))
		return 1
	}
	_, _ = fmt.Fprintf(stdout, "readiness: %d/%d items valid\n", len(items), len(items))
	return 0
}

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }
