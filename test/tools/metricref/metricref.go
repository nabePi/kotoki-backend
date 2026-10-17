// Package metricref checks that metric names used by alerts (and later dashboards) exist, and that
// the canonical list deploy/k8s/hack/metrics.txt equals what the code really registers.
//
// The registry is read from source, not from a running process: every composite literal of a
// prometheus *Opts type (CounterOpts, GaugeOpts, HistogramOpts, SummaryOpts) contributes its Name.
// Names are resolved when they are a string literal, a constant plus a literal (the realtime
// prefix), or a parameter of a local helper closure (the call sites of the helper supply the
// literal). A Name that cannot be resolved makes the scan fail instead of being skipped, so a
// metric can never silently drop out of the registry.
package metricref

import (
	"bufio"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

var (
	tokenRe  = regexp.MustCompile(`kotoki_[a-z0-9_]+`)
	nameRe   = regexp.MustCompile(`^kotoki_[a-z0-9_]+$`)
	histSufx = []string{"_bucket", "_sum", "_count"}
)

// Extract returns the distinct kotoki_ metric tokens of a PromQL expression, in order of first
// appearance. Label values such as {queue="kotoki_x"} are not stripped: callers pass expressions,
// and a quoted token is reported too (and then rejected as unknown), which is the safe direction.
func Extract(expr string) []string {
	seen := map[string]bool{}
	var out []string
	for _, t := range tokenRe.FindAllString(expr, -1) {
		if !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	return out
}

// Resolve maps a token used in an expression to the registered base name: the token itself, or
// the token without a histogram suffix (_bucket, _sum, _count) when that base is known and is not
// a *_total counter.
func Resolve(token string, known map[string]bool) (string, bool) {
	if known[token] {
		return token, true
	}
	for _, s := range histSufx {
		// a base ending in _total is a counter by naming convention, never a histogram
		if base, ok := strings.CutSuffix(token, s); ok && known[base] && !strings.HasSuffix(base, "_total") {
			return base, true
		}
	}
	return "", false
}

// LoadAllowed reads metrics.txt: one name per line, '#' comments and blank lines ignored. A '?'
// prefix marks a metric that is registered but exposes no series until a failure path runs (the
// value in the map is true for such optional metrics).
func LoadAllowed(path string) (map[string]bool, error) {
	f, err := os.Open(path) //nolint:gosec // dev tool: path is a repo file given by the caller
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	out := map[string]bool{}
	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		optional := strings.HasPrefix(line, "?")
		name := strings.TrimSpace(strings.TrimPrefix(line, "?"))
		if !nameRe.MatchString(name) {
			return nil, fmt.Errorf("%s:%d: not a metric name: %q", path, n, line)
		}
		if _, dup := out[name]; dup {
			return nil, fmt.Errorf("%s:%d: duplicate metric %q", path, n, name)
		}
		out[name] = optional
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s: no metrics listed", path)
	}
	return out, nil
}

// RegisteredInCode scans the non-test Go files under root/internal and root/cmd and returns the
// base names of every metric registered through a prometheus *Opts literal.
func RegisteredInCode(root string) (map[string]bool, error) {
	out := map[string]bool{}
	for _, dir := range []string{"internal", "cmd"} {
		base := filepath.Join(root, dir)
		err := filepath.WalkDir(base, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			names, err := scanFile(path)
			if err != nil {
				return err
			}
			for _, n := range names {
				out[n] = true
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("metricref: no metric registered under %s (wrong root?)", root)
	}
	return out, nil
}

// scanFile returns the metric names registered in one file.
func scanFile(path string) ([]string, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		return nil, fmt.Errorf("metricref: parse %s: %w", path, err)
	}
	consts := map[string]string{}
	ast.Inspect(file, func(n ast.Node) bool {
		vs, ok := n.(*ast.ValueSpec)
		if !ok {
			return true
		}
		for i, id := range vs.Names {
			if i < len(vs.Values) {
				if s, ok := stringLit(vs.Values[i]); ok {
					consts[id.Name] = s
				}
			}
		}
		return true
	})

	// helper closures: name := func(<params>) ... { ... Name: <prefix> + <param> ... }
	type helper struct {
		prefix string
		index  int // position of the name parameter
	}
	helpers := map[string]helper{}
	var names []string
	pos := func(n ast.Node) string { return fset.Position(n.Pos()).String() }
	var fail error

	// resolveName returns (literal, helper parameter index or -1, prefix, ok).
	resolveName := func(expr ast.Expr, params []*ast.Ident) (string, int, string, bool) {
		param := func(e ast.Expr) int {
			id, isID := e.(*ast.Ident)
			if !isID {
				return -1
			}
			for i, p := range params {
				if p.Name == id.Name {
					return i
				}
			}
			return -1
		}
		strOf := func(e ast.Expr) (string, bool) {
			if s, ok := stringLit(e); ok {
				return s, true
			}
			if id, isID := e.(*ast.Ident); isID {
				s, found := consts[id.Name]
				return s, found
			}
			return "", false
		}
		switch e := expr.(type) {
		case *ast.BasicLit:
			s, ok := stringLit(e)
			return s, -1, "", ok
		case *ast.Ident:
			if i := param(e); i >= 0 {
				return "", i, "", true
			}
			s, found := consts[e.Name]
			return s, -1, "", found
		case *ast.BinaryExpr:
			if e.Op != token.ADD {
				return "", -1, "", false
			}
			p, pok := strOf(e.X)
			if !pok {
				return "", -1, "", false
			}
			if i := param(e.Y); i >= 0 {
				return "", i, p, true
			}
			if s, sok := strOf(e.Y); sok {
				return p + s, -1, "", true
			}
		}
		return "", -1, "", false
	}

	// pass 1: collect helpers and direct names. A closure's parameters are only meaningful inside it.
	var visit func(n ast.Node, params []*ast.Ident, owner string)
	visit = func(n ast.Node, params []*ast.Ident, owner string) {
		ast.Inspect(n, func(c ast.Node) bool {
			if fail != nil {
				return false
			}
			switch x := c.(type) {
			case *ast.AssignStmt:
				if len(x.Lhs) == 1 && len(x.Rhs) == 1 {
					if fl, ok := x.Rhs[0].(*ast.FuncLit); ok {
						if id, ok := x.Lhs[0].(*ast.Ident); ok {
							var ps []*ast.Ident
							for _, f := range fl.Type.Params.List {
								ps = append(ps, f.Names...)
							}
							visit(fl.Body, ps, id.Name)
							return false
						}
					}
				}
			case *ast.CompositeLit:
				if !isOpts(x.Type) {
					return true
				}
				for _, el := range x.Elts {
					kv, ok := el.(*ast.KeyValueExpr)
					if !ok {
						continue
					}
					if k, ok := kv.Key.(*ast.Ident); !ok || k.Name != "Name" {
						continue
					}
					lit, idx, prefix, ok := resolveName(kv.Value, params)
					switch {
					case !ok:
						fail = fmt.Errorf("metricref: %s: cannot resolve the metric Name expression; register it with a literal or a helper closure", pos(kv))
					case idx >= 0:
						if owner == "" {
							fail = fmt.Errorf("metricref: %s: Name is a parameter outside a local helper closure", pos(kv))
						} else {
							helpers[owner] = helper{prefix: prefix, index: idx}
						}
					default:
						if nameRe.MatchString(lit) {
							names = append(names, lit)
						} else {
							fail = fmt.Errorf("metricref: %s: metric name %q does not match %s", pos(kv), lit, nameRe)
						}
					}
				}
			}
			return true
		})
	}
	visit(file, nil, "")
	if fail != nil {
		return nil, fail
	}

	// pass 2: call sites of the helpers supply the names.
	ast.Inspect(file, func(c ast.Node) bool {
		call, ok := c.(*ast.CallExpr)
		if !ok {
			return true
		}
		id, ok := call.Fun.(*ast.Ident)
		if !ok {
			return true
		}
		h, ok := helpers[id.Name]
		if !ok {
			return true
		}
		if h.index >= len(call.Args) {
			fail = fmt.Errorf("metricref: %s: helper %s called without its name argument", pos(call), id.Name)
			return false
		}
		lit, ok := stringLit(call.Args[h.index])
		if !ok {
			fail = fmt.Errorf("metricref: %s: helper %s needs a string literal metric name", pos(call), id.Name)
			return false
		}
		full := h.prefix + lit
		if !nameRe.MatchString(full) {
			fail = fmt.Errorf("metricref: %s: metric name %q does not match %s", pos(call), full, nameRe)
			return false
		}
		names = append(names, full)
		return true
	})
	if fail != nil {
		return nil, fail
	}
	sort.Strings(names)
	return names, nil
}

func isOpts(t ast.Expr) bool {
	switch x := t.(type) {
	case *ast.SelectorExpr:
		return strings.HasSuffix(x.Sel.Name, "Opts")
	case *ast.Ident:
		return strings.HasSuffix(x.Name, "Opts")
	}
	return false
}

func stringLit(e ast.Expr) (string, bool) {
	bl, ok := e.(*ast.BasicLit)
	if !ok || bl.Kind != token.STRING {
		return "", false
	}
	s, err := strconv.Unquote(bl.Value)
	return s, err == nil
}

// Sorted returns the keys of a set in order.
func Sorted(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
