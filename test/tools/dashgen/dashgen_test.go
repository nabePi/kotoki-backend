package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/nabePi/kotoki-backend/test/tools/metricref"
)

const (
	repoRoot     = "../../.."
	dashboardDir = repoRoot + "/deploy/k8s/base/observability/dashboards"
	metricsPath  = repoRoot + "/deploy/k8s/hack/metrics.txt"
)

var wantTitles = []string{"Kotoki Overview", "Realtime Latency", "Providers", "Async", "Data stores", "Business integrity"}

var wantFiles = []string{
	"kotoki-overview.json", "realtime-latency.json", "providers.json",
	"async.json", "data-stores.json", "business-integrity.json",
}

// allowedPanelTypes are the Grafana panel types the generator may emit.
var allowedPanelTypes = map[string]bool{"timeseries": true, "stat": true, "heatmap": true, "bargauge": true}

// externalMetrics are the non-kotoki_ series the dashboards may read (exporters, cAdvisor,
// kube-state-metrics, Prometheus ALERTS and the per-environment recording rules of Task 11). They
// are not verifiable against code, so the set is closed: a new one must be listed here on purpose.
var externalMetrics = map[string]bool{
	"container_cpu_usage_seconds_total":        true,
	"container_memory_working_set_bytes":       true,
	"kube_pod_container_status_restarts_total": true,
	"pg_up":                                     true,
	"pg_stat_activity_count":                    true,
	"redis_up":                                  true,
	"redis_commands_processed_total":            true,
	"redis_memory_used_bytes":                   true,
	"redis_memory_max_bytes":                    true,
	"rabbitmq_identity_info":                    true,
	"rabbitmq_process_resident_memory_bytes":    true,
	"rabbitmq_disk_space_available_bytes":       true,
	"rabbitmq_queue_messages_unacked":           true,
	"rabbitmq_alarms_free_disk_space_watermark": true,
	"rabbitmq_alarms_memory_used_watermark":     true,
	"ALERTS":                                    true,
}

var recordingRules = map[string]bool{
	"kotoki:postgres_cpu_utilization:ratio": true,
	"kotoki:redis_memory_utilization:ratio": true,
}

var (
	externalRe   = regexp.MustCompile(`\b(?:container|kube|pg|redis|rabbitmq|certmanager)_[a-z0-9_]+|\bALERTS\b`)
	recordingRe  = regexp.MustCompile(`kotoki:[a-z0-9_:]+`)
	capabilityRe = regexp.MustCompile(`capability(?:=~?|!=|!~)"([^"]*)"`)
)

func allExprs(d Dashboard) []string {
	var out []string
	for _, p := range d.Panels {
		out = append(out, p.Exprs...)
	}
	return out
}

func TestDashboardsSixTitles(t *testing.T) {
	all := All()
	var got []string
	uids := map[string]bool{}
	for _, d := range all {
		got = append(got, d.Title)
		if d.UID == "" || uids[d.UID] {
			t.Errorf("dashboard %q: uid %q empty or duplicated", d.Title, d.UID)
		}
		uids[d.UID] = true
		if len(d.Panels) < 4 {
			t.Errorf("dashboard %q has %d panels, want >= 4", d.Title, len(d.Panels))
		}
	}
	if strings.Join(got, "|") != strings.Join(wantTitles, "|") {
		t.Fatalf("titles = %q, want %q", got, wantTitles)
	}
}

func TestDashboardsUseKnownMetrics(t *testing.T) {
	allowed, err := metricref.LoadAllowed(metricsPath)
	if err != nil {
		t.Fatal(err)
	}
	registered, err := metricref.RegisteredInCode(repoRoot)
	if err != nil {
		t.Fatal(err)
	}
	// LoadAllowed maps name -> optional; Resolve wants a presence set.
	known := map[string]bool{}
	for n := range allowed {
		known[n] = true
	}
	for _, d := range All() {
		for _, p := range d.Panels {
			for _, e := range p.Exprs {
				for _, tok := range metricref.Extract(e) {
					base, ok := metricref.Resolve(tok, known)
					if !ok {
						t.Errorf("%s / %q: metric %q is not in deploy/k8s/hack/metrics.txt (expr %q)", d.Title, p.Title, tok, e)
						continue
					}
					if !registered[base] {
						t.Errorf("%s / %q: metric %q is not registered in code (expr %q)", d.Title, p.Title, base, e)
					}
					// A metric with no series until a failure path runs makes the panel empty on a
					// healthy system; the expression must supply the zero explicitly.
					if allowed[base] && !strings.Contains(e, " or ") {
						t.Errorf("%s / %q: optional metric %q needs an `or` fallback so the panel has data when healthy (expr %q)", d.Title, p.Title, base, e)
					}
				}
				for _, tok := range externalRe.FindAllString(recordingRe.ReplaceAllString(e, ""), -1) {
					if !externalMetrics[tok] {
						t.Errorf("%s / %q: external metric %q is not in the closed external set (expr %q)", d.Title, p.Title, tok, e)
					}
				}
				for _, tok := range recordingRe.FindAllString(e, -1) {
					if !recordingRules[tok] {
						t.Errorf("%s / %q: recording rule %q is unknown (expr %q)", d.Title, p.Title, tok, e)
					}
				}
			}
		}
	}
}

// balanced reports whether (), [] and {} are balanced outside string literals.
func balanced(s string) bool {
	var stack []rune
	pairs := map[rune]rune{')': '(', ']': '[', '}': '{'}
	inStr := false
	for _, r := range s {
		switch {
		case r == '"':
			inStr = !inStr
		case inStr:
		case r == '(' || r == '[' || r == '{':
			stack = append(stack, r)
		case r == ')' || r == ']' || r == '}':
			if len(stack) == 0 || stack[len(stack)-1] != pairs[r] {
				return false
			}
			stack = stack[:len(stack)-1]
		}
	}
	return len(stack) == 0 && !inStr
}

func TestPanelsNeverEmpty(t *testing.T) {
	for _, d := range All() {
		titles := map[string]bool{}
		for _, p := range d.Panels {
			if strings.TrimSpace(p.Title) == "" || titles[p.Title] {
				t.Errorf("%s: panel title %q empty or duplicated", d.Title, p.Title)
			}
			titles[p.Title] = true
			if !allowedPanelTypes[p.Type] {
				t.Errorf("%s / %q: type %q not allowed", d.Title, p.Title, p.Type)
			}
			if p.Unit == "" {
				t.Errorf("%s / %q: unit is empty (use \"short\")", d.Title, p.Title)
			}
			if len(p.Exprs) == 0 {
				t.Errorf("%s / %q: no expression, the panel would never show data", d.Title, p.Title)
			}
			seen := map[string]bool{}
			for _, e := range p.Exprs {
				if strings.TrimSpace(e) == "" {
					t.Errorf("%s / %q: blank expression", d.Title, p.Title)
				}
				if seen[e] {
					t.Errorf("%s / %q: duplicate expression %q", d.Title, p.Title, e)
				}
				seen[e] = true
				if !balanced(e) {
					t.Errorf("%s / %q: unbalanced brackets in %q", d.Title, p.Title, e)
				}
				if len(metricref.Extract(e))+len(externalRe.FindAllString(e, -1))+len(recordingRe.FindAllString(e, -1)) == 0 {
					t.Errorf("%s / %q: expression references no metric: %q", d.Title, p.Title, e)
				}
			}
		}
	}
}

func TestPanelsHaveNoForbiddenLabels(t *testing.T) {
	re := regexp.MustCompile(`\b(user_id|session_id|turn_id|tier)\b`)
	for _, d := range All() {
		for _, p := range d.Panels {
			for _, e := range p.Exprs {
				if m := re.FindString(e); m != "" {
					t.Errorf("%s / %q: forbidden label %q (cardinality / D-016) in %q", d.Title, p.Title, m, e)
				}
			}
		}
	}
}

func TestProviderCapabilitiesUppercase(t *testing.T) {
	for _, d := range All() {
		for _, e := range allExprs(d) {
			for _, m := range capabilityRe.FindAllStringSubmatch(e, -1) {
				for _, v := range strings.Split(m[1], "|") {
					if v != strings.ToUpper(v) {
						t.Errorf("%s: capability value %q must be UPPERCASE in %q", d.Title, v, e)
					}
				}
			}
		}
	}
}

func TestOverviewHasCapacityRatio(t *testing.T) {
	var ov *Dashboard
	for _, d := range All() {
		if d.Title == "Kotoki Overview" {
			d := d
			ov = &d
		}
	}
	if ov == nil {
		t.Fatal("Kotoki Overview missing")
	}
	for _, e := range allExprs(*ov) {
		if strings.Contains(e, "kotoki_realtime_sessions_active") && strings.Contains(e, "kotoki_realtime_session_capacity") && strings.Contains(e, "/") {
			return
		}
	}
	t.Fatal("Overview has no single expression dividing kotoki_realtime_sessions_active by kotoki_realtime_session_capacity")
}

func TestRealtimeLatencyHasRequiredPanels(t *testing.T) {
	var rl Dashboard
	for _, d := range All() {
		if d.Title == "Realtime Latency" {
			rl = d
		}
	}
	joined := strings.Join(allExprs(rl), "\n")
	for _, m := range []string{
		"kotoki_realtime_turn_end_to_first_audio_seconds", "kotoki_realtime_stt_final_seconds",
		"kotoki_realtime_llm_first_sentence_seconds", "kotoki_realtime_tts_first_audio_seconds",
		"kotoki_realtime_server_overhead_seconds", "kotoki_realtime_bargein_stop_seconds",
		"kotoki_realtime_frame_processing_seconds", "kotoki_realtime_slow_consumer_total",
	} {
		if !strings.Contains(joined, m) {
			t.Errorf("Realtime Latency lacks a panel for %s", m)
		}
	}
	hasHeatmap := false
	for _, p := range rl.Panels {
		if p.Type == "heatmap" && strings.Contains(strings.Join(p.Exprs, ""), "turn_end_to_first_audio_seconds_bucket") {
			hasHeatmap = true
		}
	}
	if !hasHeatmap {
		t.Error("Realtime Latency lacks the turn_end_to_first_audio heatmap")
	}
}

func TestRenderDeterministic(t *testing.T) {
	for i, d := range All() {
		a, err := Render(d)
		if err != nil {
			t.Fatal(err)
		}
		b, err := Render(All()[i])
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(a, b) {
			t.Errorf("%s: Render is not deterministic", d.Title)
		}
		if !json.Valid(a) || !bytes.HasSuffix(a, []byte("}\n")) {
			t.Errorf("%s: output is not valid JSON ending in a newline", d.Title)
		}
	}
}

func TestRunWritesIdempotently(t *testing.T) {
	dir := t.TempDir()
	if err := run(dir); err != nil {
		t.Fatal(err)
	}
	first := readDir(t, dir)
	if err := run(dir); err != nil {
		t.Fatal(err)
	}
	second := readDir(t, dir)
	if len(first) != 6 {
		t.Fatalf("wrote %d files, want 6", len(first))
	}
	for name, b := range first {
		if !bytes.Equal(b, second[name]) {
			t.Errorf("%s differs between two runs", name)
		}
	}
	if err := run(filepath.Join(dir, "missing", "dir")); err != nil {
		t.Fatalf("run must create the output directory: %v", err)
	}
}

func readDir(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string][]byte{}
	for _, e := range ents {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		out[e.Name()] = b
	}
	return out
}

func TestRenderedJSONUpToDate(t *testing.T) {
	onDisk := readDir(t, dashboardDir)
	var names []string
	for n := range onDisk {
		names = append(names, n)
	}
	sort.Strings(names)
	if strings.Join(names, ",") != strings.Join(sortedCopy(wantFiles), ",") {
		t.Fatalf("dashboards dir has %v, want exactly %v", names, wantFiles)
	}
	for i, d := range All() {
		want, err := Render(d)
		if err != nil {
			t.Fatal(err)
		}
		if got := onDisk[wantFiles[i]]; !bytes.Equal(got, want) {
			t.Errorf("%s is stale; run `make dashboards` and commit", wantFiles[i])
		}
	}
}

func sortedCopy(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

// TestRenderedJSONStructure validates the committed files themselves (not the generator): schema
// version, a consistent Prometheus datasource variable, unique ids, a non-overlapping 24-column
// grid and targets that carry exactly the panel expressions.
func TestRenderedJSONStructure(t *testing.T) {
	for i, f := range wantFiles {
		b, err := os.ReadFile(filepath.Join(dashboardDir, f))
		if err != nil {
			t.Fatal(err)
		}
		var dash struct {
			UID           string `json:"uid"`
			Title         string `json:"title"`
			SchemaVersion int    `json:"schemaVersion"`
			Templating    struct {
				List []struct {
					Name  string `json:"name"`
					Type  string `json:"type"`
					Query string `json:"query"`
				} `json:"list"`
			} `json:"templating"`
			Panels []struct {
				ID         int     `json:"id"`
				Type       string  `json:"type"`
				Title      string  `json:"title"`
				GridPos    gridPos `json:"gridPos"`
				Datasource struct {
					Type string `json:"type"`
					UID  string `json:"uid"`
				} `json:"datasource"`
				Targets []struct {
					RefID      string `json:"refId"`
					Expr       string `json:"expr"`
					Datasource struct {
						Type string `json:"type"`
						UID  string `json:"uid"`
					} `json:"datasource"`
				} `json:"targets"`
			} `json:"panels"`
		}
		if err := json.Unmarshal(b, &dash); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		spec := All()[i]
		if dash.UID != spec.UID || dash.Title != spec.Title {
			t.Errorf("%s: uid/title = %q/%q, want %q/%q", f, dash.UID, dash.Title, spec.UID, spec.Title)
		}
		if dash.SchemaVersion < 39 {
			t.Errorf("%s: schemaVersion %d < 39", f, dash.SchemaVersion)
		}
		hasDS := false
		for _, v := range dash.Templating.List {
			if v.Name == "DS_PROMETHEUS" && v.Type == "datasource" && v.Query == "prometheus" {
				hasDS = true
			}
		}
		if !hasDS {
			t.Errorf("%s: no datasource variable DS_PROMETHEUS (type datasource, query prometheus)", f)
		}
		if len(dash.Panels) != len(spec.Panels) {
			t.Fatalf("%s: %d panels, want %d", f, len(dash.Panels), len(spec.Panels))
		}
		ids := map[int]bool{}
		for j, p := range dash.Panels {
			if p.ID <= 0 || ids[p.ID] {
				t.Errorf("%s / %q: id %d invalid or duplicated", f, p.Title, p.ID)
			}
			ids[p.ID] = true
			if p.Datasource.UID != "${DS_PROMETHEUS}" || p.Datasource.Type != "prometheus" {
				t.Errorf("%s / %q: panel datasource = %+v", f, p.Title, p.Datasource)
			}
			if p.Type != spec.Panels[j].Type || p.Title != spec.Panels[j].Title {
				t.Errorf("%s: panel %d = %q/%q, want %q/%q", f, j, p.Type, p.Title, spec.Panels[j].Type, spec.Panels[j].Title)
			}
			if len(p.Targets) != len(spec.Panels[j].Exprs) || len(p.Targets) == 0 {
				t.Errorf("%s / %q: %d targets, want %d (>0)", f, p.Title, len(p.Targets), len(spec.Panels[j].Exprs))
				continue
			}
			refs := map[string]bool{}
			for k, tg := range p.Targets {
				if tg.Expr != spec.Panels[j].Exprs[k] || strings.TrimSpace(tg.Expr) == "" {
					t.Errorf("%s / %q: target %d expr %q", f, p.Title, k, tg.Expr)
				}
				if tg.Datasource.UID != "${DS_PROMETHEUS}" || refs[tg.RefID] || tg.RefID == "" {
					t.Errorf("%s / %q: target %d datasource/refId invalid (%+v, %q)", f, p.Title, k, tg.Datasource, tg.RefID)
				}
				refs[tg.RefID] = true
			}
			g := p.GridPos
			if g.W <= 0 || g.H <= 0 || g.X < 0 || g.Y < 0 || g.X+g.W > 24 {
				t.Errorf("%s / %q: gridPos %+v outside the 24-column grid", f, p.Title, g)
			}
			for k := 0; k < j; k++ {
				o := dash.Panels[k].GridPos
				if g.X < o.X+o.W && o.X < g.X+g.W && g.Y < o.Y+o.H && o.Y < g.Y+g.H {
					t.Errorf("%s: panels %q and %q overlap", f, p.Title, dash.Panels[k].Title)
				}
			}
		}
	}
}

func TestDashboardsKustomization(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(dashboardDir, "kustomization.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var k struct {
		Kind      string `yaml:"kind"`
		Generator []struct {
			Name      string   `yaml:"name"`
			Namespace string   `yaml:"namespace"`
			Files     []string `yaml:"files"`
			Options   struct {
				Disable bool              `yaml:"disableNameSuffixHash"`
				Labels  map[string]string `yaml:"labels"`
			} `yaml:"options"`
		} `yaml:"configMapGenerator"`
	}
	if err := yaml.Unmarshal(b, &k); err != nil {
		t.Fatal(err)
	}
	if k.Kind != "Kustomization" || len(k.Generator) != 1 {
		t.Fatalf("want one configMapGenerator in a Kustomization, got kind %q, %d generators", k.Kind, len(k.Generator))
	}
	g := k.Generator[0]
	if g.Name != "kotoki-dashboards" || g.Namespace != "observability" {
		t.Errorf("generator = %q in %q, want kotoki-dashboards in observability", g.Name, g.Namespace)
	}
	if !g.Options.Disable {
		t.Error("disableNameSuffixHash must be true")
	}
	if g.Options.Labels["grafana_dashboard"] != "1" {
		t.Errorf("label grafana_dashboard = %q, want \"1\" (Grafana sidecar)", g.Options.Labels["grafana_dashboard"])
	}
	if strings.Join(sortedCopy(g.Files), ",") != strings.Join(sortedCopy(wantFiles), ",") {
		t.Errorf("files = %v, want %v", g.Files, wantFiles)
	}
}
