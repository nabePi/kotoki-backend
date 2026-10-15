// Command migcheck statically validates the migrations directory (spec §14.7, §15.1).
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

var canonical = []string{"extensions", "users", "auth_tokens", "balance_accounts", "topics", "scenarios", "characters", "prompts", "sessions", "session_turns", "session_events", "balance_reservations", "balance_ledger", "learner", "memories", "evaluations", "messaging", "admin_audit"}

var fileRE = regexp.MustCompile(`^(\d+)_([a-z0-9_]+)\.(up|down)\.sql$`)

// checkFiles validates migration file names (base names only).
func checkFiles(names []string) error {
	type pair struct {
		name     string
		up, down bool
	}
	byNum := map[int]*pair{}
	for _, n := range names {
		m := fileRE.FindStringSubmatch(n)
		if m == nil {
			return fmt.Errorf("%s: not NNNNNN_name.(up|down).sql", n)
		}
		if len(m[1]) != 6 {
			return fmt.Errorf("%s: version must have 6 digits", n)
		}
		num, _ := strconv.Atoi(m[1])
		p := byNum[num]
		if p == nil {
			p = &pair{name: m[2]}
			byNum[num] = p
		}
		if p.name != m[2] {
			return fmt.Errorf("%06d: up/down names differ (%s vs %s)", num, p.name, m[2])
		}
		if m[3] == "up" {
			p.up = true
		} else {
			p.down = true
		}
	}
	nums := make([]int, 0, len(byNum))
	for n := range byNum {
		nums = append(nums, n)
	}
	sort.Ints(nums)
	for i, n := range nums {
		want := i + 1
		if n != want {
			return fmt.Errorf("migration %06d missing (found %06d next): versions must be consecutive", want, n)
		}
		p := byNum[n]
		if !p.up {
			return fmt.Errorf("%06d_%s: missing .up.sql", n, p.name)
		}
		if !p.down {
			return fmt.Errorf("%06d_%s: missing .down.sql", n, p.name)
		}
		if i < len(canonical) && p.name != canonical[i] {
			return fmt.Errorf("%06d_%s: canonical name is %06d_%s", n, p.name, n, canonical[i])
		}
	}
	return nil
}

// modifiedGuard rejects edits to migrations already on the base branch once v0.1.0 exists.
func modifiedGuard(tagExists bool, modified []string) error {
	if !tagExists || len(modified) == 0 {
		return nil
	}
	return fmt.Errorf("migrations modified after tag v0.1.0 are immutable: %s", strings.Join(modified, ", "))
}

// changedMigrations lists migrations on base that were edited or removed. Renames are
// reported as delete+add (--no-renames) so renaming an applied migration away is caught.
func changedMigrations(repo, base, dir string) ([]string, error) {
	out, err := git("-C", repo, "diff", "--name-only", "--no-renames", "--diff-filter=MD", base, "--", dir)
	if err != nil {
		return nil, err
	}
	if out == "" {
		return nil, nil
	}
	return strings.Split(out, "\n"), nil
}

func git(args ...string) (string, error) {
	out, err := exec.CommandContext(context.Background(), "git", args...).Output() //nolint:gosec // dev CLI: fixed "git" binary, arguments from our own flags
	return strings.TrimSpace(string(out)), err
}

func main() {
	dir := flag.String("dir", "migrations", "migrations directory")
	base := flag.String("base", "origin/main", "base ref for the immutability check")
	flag.Parse()

	entries, err := filepath.Glob(filepath.Join(*dir, "*.sql"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "migcheck:", err)
		os.Exit(2)
	}
	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = filepath.Base(e)
	}
	if err := checkFiles(names); err != nil {
		fmt.Fprintln(os.Stderr, "migcheck:", err)
		os.Exit(1)
	}
	if _, err := git("rev-parse", "--verify", "--quiet", *base); err != nil {
		fmt.Println("migcheck: base ref", *base, "not found, immutability check skipped")
		return
	}
	tag, _ := git("tag", "--list", "v0.1.0")
	modified, err := changedMigrations(".", *base, *dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "migcheck: diff against", *base, "failed:", err)
		os.Exit(2)
	}
	if err := modifiedGuard(tag != "", modified); err != nil {
		fmt.Fprintln(os.Stderr, "migcheck:", err)
		os.Exit(1)
	}
}
