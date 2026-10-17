package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const root = "../../.."

func TestMain(m *testing.M) {
	repoRoot = root
	os.Exit(m.Run())
}

func realItems(t *testing.T) []string {
	t.Helper()
	items, err := loadItems("items.txt")
	if err != nil {
		t.Fatal(err)
	}
	return items
}

// doc builds a minimal readiness table for items; row(i) returns the Status and Evidence cells.
func doc(items []string, row func(i int, item string) (string, string)) string {
	var b strings.Builder
	b.WriteString("# Kesiapan produksi\n\n| Butir | Status | Bukti |\n|---|---|---|\n")
	for i, it := range items {
		s, e := row(i, it)
		fmt.Fprintf(&b, "| %s | %s | %s |\n", it, s, e)
	}
	return b.String()
}

var okRow = func(int, string) (string, string) { return "EXTERNAL", "tim infra menyediakan" }

func errsContain(errs []error, sub string) bool {
	for _, e := range errs {
		if strings.Contains(e.Error(), sub) {
			return true
		}
	}
	return false
}

func TestItemsFileHas29UniqueItems(t *testing.T) {
	items := realItems(t)
	if len(items) != 29 {
		t.Fatalf("items.txt has %d items, want 29 (27 of §13.14.1 + 2 derived)", len(items))
	}
	seen := map[string]bool{}
	for _, it := range items {
		if seen[it] {
			t.Errorf("duplicate item %q", it)
		}
		seen[it] = true
		if strings.Contains(it, "|") {
			t.Errorf("item %q contains a table separator", it)
		}
	}
}

func TestCheck_AllItemsPresent(t *testing.T) {
	items := realItems(t)
	if errs := Check(doc(items, okRow), items); len(errs) != 0 {
		t.Fatalf("a complete document must pass, got %v", errs)
	}
}

func TestCheck_MissingItemFails(t *testing.T) {
	items := realItems(t)
	md := doc(items[1:], okRow)
	errs := Check(md, items)
	if !errsContain(errs, items[0]) || !errsContain(errs, "missing") {
		t.Fatalf("missing item must be reported, got %v", errs)
	}
}

func TestCheck_DuplicateAndUnknownItemFail(t *testing.T) {
	items := realItems(t)
	md := doc(items, okRow) + fmt.Sprintf("| %s | EXTERNAL | x |\n| Butir asing | EXTERNAL | x |\n", items[3])
	errs := Check(md, items)
	if !errsContain(errs, "more than once") || !errsContain(errs, "unknown item") {
		t.Fatalf("duplicate and unknown rows must be reported, got %v", errs)
	}
}

func TestCheck_InvalidStatusFails(t *testing.T) {
	items := realItems(t)
	md := doc(items, func(i int, _ string) (string, string) {
		if i == 2 {
			return "SELESAI", "bukti"
		}
		return okRow(i, "")
	})
	if errs := Check(md, items); !errsContain(errs, "invalid status") {
		t.Fatalf("invalid status must fail, got %v", errs)
	}
}

func TestCheck_DoneWithoutEvidenceFails(t *testing.T) {
	items := realItems(t)
	for _, ev := range []string{"", "  ", "-", "TBD"} {
		md := doc(items, func(i int, _ string) (string, string) {
			if i == 4 {
				return "DONE", ev
			}
			return okRow(i, "")
		})
		if errs := Check(md, items); !errsContain(errs, "evidence") {
			t.Errorf("DONE with evidence %q must fail, got %v", ev, errs)
		}
	}
}

func TestCheck_DoneNeedsAPathAndEvidencePathMustExist(t *testing.T) {
	items := realItems(t)
	cases := []struct {
		name, status, ev, want string
	}{
		{"prose only", "DONE", "sudah dites", "path"},
		{"missing path", "DONE", "`docs/tidak-ada/x.md`", "does not exist"},
		{"missing path with anchor", "LOCAL-VERIFIED", "`go test ./x`; `test/tidak/ada_test.go:TestX`", "does not exist"},
	}
	for _, c := range cases {
		md := doc(items, func(i int, _ string) (string, string) {
			if i == 0 {
				return c.status, c.ev
			}
			return okRow(i, "")
		})
		if errs := Check(md, items); !errsContain(errs, c.want) {
			t.Errorf("%s: want error containing %q, got %v", c.name, c.want, errs)
		}
	}
	good := doc(items, func(i int, _ string) (string, string) {
		if i == 0 {
			return "DONE", "`deploy/k8s/hack/pins-check.sh`, `test/tools/readinesscheck/items.txt:Database`"
		}
		return okRow(i, "")
	})
	if errs := Check(good, items); len(errs) != 0 {
		t.Fatalf("existing paths must pass, got %v", errs)
	}
}

func TestCheck_ManualGateNeedsMGAndLocalNeedsCommand(t *testing.T) {
	items := realItems(t)
	md := doc(items, func(i int, _ string) (string, string) {
		switch i {
		case 0:
			return "MANUAL-GATE", "perlu klaster"
		case 1:
			return "LOCAL-VERIFIED", "`deploy/k8s/hack/pins-check.sh`"
		}
		return okRow(i, "")
	})
	errs := Check(md, items)
	if !errsContain(errs, "MG-") || !errsContain(errs, "command") {
		t.Fatalf("MANUAL-GATE without MG-n and LOCAL-VERIFIED without a command must fail, got %v", errs)
	}
	ok := doc(items, func(i int, _ string) (string, string) {
		switch i {
		case 0:
			return "MANUAL-GATE", "MG-1 (staging)"
		case 1:
			return "LOCAL-VERIFIED", "`go test ./test/tools/...`; `deploy/k8s/hack/pins-check.sh`"
		}
		return okRow(i, "")
	})
	if errs := Check(ok, items); len(errs) != 0 {
		t.Fatalf("valid MANUAL-GATE/LOCAL-VERIFIED rows must pass, got %v", errs)
	}
}

func TestCheck_NoTableFails(t *testing.T) {
	if errs := Check("# kosong\n", []string{"a"}); len(errs) == 0 {
		t.Fatal("a document without rows must fail")
	}
}

func TestProductionReadiness_RealDocument(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(root, "docs/production-readiness.md"))
	if err != nil {
		t.Fatal(err)
	}
	if errs := Check(string(b), realItems(t)); len(errs) != 0 {
		t.Fatalf("docs/production-readiness.md: %v", errs)
	}
}

// ---- runbooks ----

var runbookSlug = map[string]string{
	"R1": "turn-latency", "R2": "provider-degraded", "R3": "async-pipeline", "R4": "redis", "R5": "postgres",
	"R6": "realtime-capacity", "R7": "balance-invariant", "R8": "secret-leak", "R9": "jobs-stale",
	"R10": "vendor-switch", "R11": "gateway-certificate",
}

func runbook(t *testing.T, id string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, "docs/runbook", id+"-"+runbookSlug[id]+".md"))
	if err != nil {
		t.Fatalf("runbook %s: %v", id, err)
	}
	return string(b)
}

type ruleDoc struct {
	Spec struct {
		Groups []struct {
			Rules []struct {
				Alert       string            `yaml:"alert"`
				Annotations map[string]string `yaml:"annotations"`
			} `yaml:"rules"`
		} `yaml:"groups"`
	} `yaml:"spec"`
}

// TestRunbooksCoverAlerts: every `runbook: R<n>` annotation of prometheusrule.yaml has a file that
// carries the matching `Alert: <name>` line, and every `Alert:` line names a real alert of that runbook.
// RUNBOOK_CHECK=off skips it (default on).
func TestRunbooksCoverAlerts(t *testing.T) {
	if os.Getenv("RUNBOOK_CHECK") == "off" {
		t.Skip("RUNBOOK_CHECK=off")
	}
	b, err := os.ReadFile(filepath.Join(root, "deploy/k8s/base/observability/prometheusrule.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var rd ruleDoc
	if err := yaml.Unmarshal(b, &rd); err != nil {
		t.Fatal(err)
	}
	want := map[string]map[string]bool{}
	n := 0
	for _, g := range rd.Spec.Groups {
		for _, r := range g.Rules {
			n++
			rb := r.Annotations["runbook"]
			if want[rb] == nil {
				want[rb] = map[string]bool{}
			}
			want[rb][r.Alert] = true
		}
	}
	if n != 34 {
		t.Fatalf("prometheusrule.yaml has %d alerts, want 34", n)
	}
	alertLine := regexp.MustCompile(`(?m)^Alert: ([A-Za-z0-9]+)\s*$`)
	for rb, alerts := range want {
		if _, ok := runbookSlug[rb]; !ok {
			t.Errorf("runbook %q is outside R1..R11", rb)
			continue
		}
		have := map[string]bool{}
		for _, m := range alertLine.FindAllStringSubmatch(runbook(t, rb), -1) {
			have[m[1]] = true
		}
		for a := range alerts {
			if !have[a] {
				t.Errorf("%s: missing line %q", rb, "Alert: "+a)
			}
		}
		for a := range have {
			if !alerts[a] {
				t.Errorf("%s: `Alert: %s` is not an alert pointing to this runbook", rb, a)
			}
		}
	}
	// R10 has no alert pointing to it (manual vendor switch); it must still exist.
	for _, id := range sortedKeys(runbookSlug) {
		if _, err := os.Stat(filepath.Join(root, "docs/runbook", id+"-"+runbookSlug[id]+".md")); err != nil {
			t.Errorf("%s: %v", id, err)
		}
	}
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func TestRunbookSections(t *testing.T) {
	for _, id := range sortedKeys(runbookSlug) {
		md := runbook(t, id)
		last := -1
		for _, h := range []string{"## Gejala", "## Diagnosis cepat", "## Mitigasi", "## Verifikasi pulih"} {
			idx := strings.Index(md, "\n"+h+"\n")
			if idx < 0 {
				t.Errorf("%s: heading %q missing", id, h)
				continue
			}
			if idx < last {
				t.Errorf("%s: heading %q out of order", id, h)
			}
			last = idx
		}
		if !strings.HasPrefix(md, "# "+id+" ") {
			t.Errorf("%s: first line must be `# %s <title>`", id, id)
		}
	}
}

// TestRunbooksNameRealAlerts: alert names used in `Alert:` lines exist in alert-names.txt, and the
// operator tools the runbooks tell people to run exist in the repo.
func TestRunbooksUseRealCommands(t *testing.T) {
	names, err := os.ReadFile(filepath.Join(root, "deploy/k8s/hack/alert-names.txt"))
	if err != nil {
		t.Fatal(err)
	}
	known := map[string]bool{}
	for _, l := range strings.Fields(string(names)) {
		known[l] = true
	}
	alertLine := regexp.MustCompile(`(?m)^Alert: ([A-Za-z0-9]+)\s*$`)
	for _, id := range sortedKeys(runbookSlug) {
		md := runbook(t, id)
		for _, m := range alertLine.FindAllStringSubmatch(md, -1) {
			if !known[m[1]] {
				t.Errorf("%s: alert %q is not in alert-names.txt", id, m[1])
			}
		}
		for _, bad := range []string{"<PIN>", "TODO", "TBD"} {
			if strings.Contains(md, bad) {
				t.Errorf("%s: contains %q", id, bad)
			}
		}
	}
	// Dashboards named in runbooks must be real dashboard titles.
	titles := []string{"Kotoki Overview", "Realtime Latency", "Providers", "Async", "Data stores", "Business integrity"}
	re := regexp.MustCompile("Dashboard `([^`]+)`")
	for _, id := range sortedKeys(runbookSlug) {
		for _, m := range re.FindAllStringSubmatch(runbook(t, id), -1) {
			ok := false
			for _, tt := range titles {
				ok = ok || tt == m[1]
			}
			if !ok {
				t.Errorf("%s: dashboard %q is not one of %v", id, m[1], titles)
			}
		}
	}
}
