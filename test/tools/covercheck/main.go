// Command covercheck enforces per-package coverage thresholds (spec §14.8) on a
// `-coverprofile` file and, optionally, forbids drops > 1 point against a baseline.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
)

const maxDrop = 1.0

type stat struct{ total, covered int }

func (s stat) pct() float64 {
	if s.total == 0 {
		return 100
	}
	return 100 * float64(s.covered) / float64(s.total)
}

// parse returns statement counts per package directory (module-relative, e.g. internal/balance).
func parse(r io.Reader) (map[string]*stat, error) {
	type block struct{ n, c int }
	blocks := map[string]block{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "mode:") {
			continue
		}
		sp := strings.Fields(line)
		if len(sp) != 3 {
			return nil, fmt.Errorf("bad profile line %q", line)
		}
		n, err1 := strconv.Atoi(sp[1])
		c, err2 := strconv.Atoi(sp[2])
		if err1 != nil || err2 != nil {
			return nil, fmt.Errorf("bad profile line %q", line)
		}
		b := blocks[sp[0]]
		b.n = n
		b.c += c
		blocks[sp[0]] = b
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	out := map[string]*stat{}
	for key, b := range blocks {
		file := key[:strings.LastIndex(key, ":")]
		dir := file[:strings.LastIndex(file, "/")]
		if i := strings.Index(dir, "internal/"); i >= 0 {
			dir = dir[i:]
		} else if i := strings.Index(dir, "cmd/"); i >= 0 {
			dir = dir[i:]
		}
		s := out[dir]
		if s == nil {
			s = &stat{}
			out[dir] = s
		}
		s.total += b.n
		if b.c > 0 {
			s.covered += b.n
		}
	}
	return out, nil
}

func underPrefix(dir, prefix string) bool { return dir == prefix || strings.HasPrefix(dir, prefix+"/") }

func aggregate(pkgs map[string]*stat, prefix string, exclude func(string) bool) (stat, bool) {
	var agg stat
	seen := false
	for dir, s := range pkgs {
		if !underPrefix(dir, prefix) || (exclude != nil && exclude(dir)) {
			continue
		}
		seen = true
		agg.total += s.total
		agg.covered += s.covered
	}
	return agg, seen
}

func excludedFromTotal(dir string) bool {
	return underPrefix(dir, "internal/postgres/sqlcgen") || underPrefix(dir, "internal/testutil")
}

func check(profile, thresholds, baseline io.Reader) ([]string, error) {
	pkgs, err := parse(profile)
	if err != nil {
		return nil, err
	}
	var fails []string
	sc := bufio.NewScanner(thresholds)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Fields(line)
		total := f[0] == "TOTAL"
		var except []string
		if total && len(f) == 4 && strings.HasPrefix(f[3], "except=") {
			except = strings.Split(strings.TrimPrefix(f[3], "except="), ",")
			f = f[:3]
		}
		if (total && len(f) != 3) || (!total && len(f) != 2) {
			return nil, fmt.Errorf("bad threshold line %q", line)
		}
		prefix, minStr := f[0], f[1]
		if total {
			prefix, minStr = f[1], f[2]
		}
		floor, err := strconv.ParseFloat(strings.TrimSuffix(minStr, "%"), 64)
		if err != nil {
			return nil, fmt.Errorf("bad threshold line %q", line)
		}
		var ex func(string) bool
		if total {
			ex = func(dir string) bool {
				if excludedFromTotal(dir) {
					return true
				}
				for _, e := range except {
					if underPrefix(dir, e) {
						return true
					}
				}
				return false
			}
		}
		agg, seen := aggregate(pkgs, prefix, ex)
		if !seen {
			continue
		}
		if agg.pct() < floor {
			label := prefix
			if total {
				label = "TOTAL " + prefix
			}
			fails = append(fails, fmt.Sprintf("%s: coverage %.1f%% < %.0f%%", label, agg.pct(), floor))
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if baseline != nil {
		base, err := parse(baseline)
		if err != nil {
			return nil, err
		}
		dirs := make([]string, 0, len(base))
		for d := range base {
			dirs = append(dirs, d)
		}
		sort.Strings(dirs)
		for _, d := range dirs {
			cur, ok := pkgs[d]
			if !ok {
				continue
			}
			if drop := base[d].pct() - cur.pct(); drop > maxDrop {
				fails = append(fails, fmt.Sprintf("%s: coverage dropped %.1f points (%.1f%% -> %.1f%%)", d, drop, base[d].pct(), cur.pct()))
			}
		}
	}
	return fails, nil
}

func main() { os.Exit(run()) }

func run() int {
	profile := flag.String("profile", "coverage.out", "coverage profile")
	thresholds := flag.String("thresholds", "test/tools/covercheck/thresholds.txt", "threshold file")
	baseline := flag.String("baseline", "", "optional coverage profile of main")
	flag.Parse()

	fail := func(err error) int {
		fmt.Fprintln(os.Stderr, "covercheck:", err)
		return 2
	}
	pf, err := os.Open(*profile)
	if err != nil {
		return fail(err)
	}
	defer func() { _ = pf.Close() }()
	tf, err := os.Open(*thresholds)
	if err != nil {
		return fail(err)
	}
	defer func() { _ = tf.Close() }()
	var bf io.Reader
	if *baseline != "" {
		f, err := os.Open(*baseline)
		if err != nil {
			return fail(err)
		}
		defer func() { _ = f.Close() }()
		bf = f
	}
	fails, err := check(pf, tf, bf)
	if err != nil {
		return fail(err)
	}
	for _, f := range fails {
		fmt.Println("FAIL", f)
	}
	if len(fails) > 0 {
		return 1
	}
	return 0
}
