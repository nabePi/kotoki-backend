// Command modelcheck fails when production Go code hard-codes a vendor model
// name in a string literal (model names belong in config and the DB).
package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// modelRE is the spec 13 pattern, anchored so that it only matches at the start of a word:
// "subscribe" (scribe), "SubscribePod" or "think-" are not model names.
var modelRE = regexp.MustCompile(`(^|[^A-Za-z0-9])(gpt-|o[0-9]-|deepseek-|claude-|eleven_|scribe|sonic|ink-|text-embedding-)`)

func skip(path string) bool {
	p := filepath.ToSlash(path)
	return strings.HasSuffix(p, "_test.go") ||
		strings.Contains(p, "internal/platform/config/") ||
		strings.Contains(p, "internal/testutil/") || // test support, kept out of production by depguard
		strings.Contains(p, "/testdata/") || strings.HasPrefix(p, "testdata/")
}

func scan(roots []string) ([]string, error) {
	var out []string
	fset := token.NewFileSet()
	for _, root := range roots {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error { //nolint:gosec // dev CLI: roots are directories given on its own command line
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || skip(path) {
				return nil
			}
			f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
			if err != nil {
				return err
			}
			// Struct tags carrying envDefault are the model defaults of config structs (spec 13, the
			// exception for the defaults of spec 8.5.1); every other literal is checked.
			tags := map[*ast.BasicLit]bool{}
			ast.Inspect(f, func(n ast.Node) bool {
				if fld, ok := n.(*ast.Field); ok && fld.Tag != nil && strings.Contains(fld.Tag.Value, "envDefault:") {
					tags[fld.Tag] = true
				}
				return true
			})
			ast.Inspect(f, func(n ast.Node) bool {
				lit, ok := n.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING || tags[lit] {
					return true
				}
				s, err := strconv.Unquote(lit.Value)
				if err != nil {
					s = lit.Value
				}
				if modelRE.MatchString(s) {
					pos := fset.Position(lit.Pos())
					out = append(out, fmt.Sprintf("%s:%d: model name literal %s", pos.Filename, pos.Line, lit.Value))
				}
				return true
			})
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	sort.Strings(out)
	return out, nil
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		args = []string{"./internal", "./cmd"}
	}
	roots := args[:0:0]
	for _, a := range args {
		if _, err := os.Stat(a); err == nil { //nolint:gosec // dev CLI: roots are directories given on its own command line
			roots = append(roots, a)
		}
	}
	found, err := scan(roots)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "modelcheck:", err)
		return 2
	}
	for _, f := range found {
		_, _ = fmt.Fprintln(stdout, f)
	}
	if len(found) > 0 {
		return 1
	}
	return 0
}

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }
