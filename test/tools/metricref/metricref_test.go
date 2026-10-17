package metricref

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const (
	repoRoot    = "../../.."
	rulesPath   = repoRoot + "/deploy/k8s/base/observability/prometheusrule.yaml"
	metricsPath = repoRoot + "/deploy/k8s/hack/metrics.txt"
	namesPath   = repoRoot + "/deploy/k8s/hack/alert-names.txt"
	monitorPath = repoRoot + "/deploy/k8s/base/observability/podmonitor.yaml"
	runbookDir  = repoRoot + "/docs/runbook"
	runbookBase = "https://github.com/nabePi/kotoki-backend/blob/main/docs/runbook/"
)

type rule struct {
	Alert       string            `yaml:"alert"`
	Expr        string            `yaml:"expr"`
	For         string            `yaml:"for"`
	Labels      map[string]string `yaml:"labels"`
	Annotations map[string]string `yaml:"annotations"`
}

type ruleFile struct {
	Kind string `yaml:"kind"`
	Spec struct {
		Groups []struct {
			Name  string `yaml:"name"`
			Rules []rule `yaml:"rules"`
		} `yaml:"groups"`
	} `yaml:"spec"`
}

func loadRules(t *testing.T) []rule {
	t.Helper()
	b, err := os.ReadFile(rulesPath)
	if err != nil {
		t.Fatalf("prometheusrule.yaml: %v", err)
	}
	var f ruleFile
	if err := yaml.Unmarshal(b, &f); err != nil {
		t.Fatalf("prometheusrule.yaml: %v", err)
	}
	if f.Kind != "PrometheusRule" {
		t.Fatalf("kind = %q, want PrometheusRule", f.Kind)
	}
	var out []rule
	for _, g := range f.Spec.Groups {
		out = append(out, g.Rules...)
	}
	if len(out) == 0 {
		t.Fatal("prometheusrule.yaml has no rules")
	}
	return out
}

func readLines(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, l := range strings.Split(string(b), "\n") {
		if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "#") {
			out = append(out, l)
		}
	}
	return out
}

// owner names the milestone that must register a metric (plan M10 Task 8).
func owner(name string) string {
	for _, p := range []struct{ prefix, ms string }{
		{"kotoki_http_", "M2"}, {"kotoki_auth_", "M2"}, {"kotoki_ratelimit_", "M2"},
		{"kotoki_sessions_", "M3"}, {"kotoki_content_", "M3"}, {"kotoki_session_", "M3"},
		{"kotoki_provider_", "M4"}, {"kotoki_stt_", "M4"}, {"kotoki_tts_", "M4"},
		{"kotoki_realtime_", "M5"}, {"kotoki_llm_", "M5"},
		{"kotoki_outbox_", "M6"}, {"kotoki_job_", "M6"}, {"kotoki_rabbitmq_", "M6"}, {"kotoki_sweeper_", "M6"},
		{"kotoki_balance_", "M6"}, {"kotoki_partition", "M6"}, {"kotoki_consumer_", "M6"},
		{"kotoki_evaluation_", "M7"}, {"kotoki_memory_", "M8"}, {"kotoki_learner_", "M8"}, {"kotoki_summary_", "M8"},
		{"kotoki_db_", "M1"}, {"kotoki_redis_", "M1"},
	} {
		if strings.HasPrefix(name, p.prefix) {
			return p.ms
		}
	}
	return "unknown milestone"
}

func TestExtract(t *testing.T) {
	got := Extract(`histogram_quantile(0.95, sum by (le) (rate(kotoki_realtime_server_overhead_seconds_bucket[5m]))) > 0.15 and kotoki_x_total > 1 and kotoki_realtime_server_overhead_seconds_bucket > 0 or pg_up == 0 or kotoki:redis_memory_utilization:ratio > 1`)
	want := []string{"kotoki_realtime_server_overhead_seconds_bucket", "kotoki_x_total"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Extract = %v, want %v", got, want)
	}
	if got := Extract(`pg_up == 0`); len(got) != 0 {
		t.Fatalf("Extract(non-kotoki) = %v", got)
	}
}

func TestResolve(t *testing.T) {
	known := map[string]bool{"kotoki_h_seconds": true, "kotoki_c_total": true}
	for tok, want := range map[string]string{
		"kotoki_h_seconds": "kotoki_h_seconds", "kotoki_h_seconds_bucket": "kotoki_h_seconds",
		"kotoki_h_seconds_sum": "kotoki_h_seconds", "kotoki_h_seconds_count": "kotoki_h_seconds", "kotoki_c_total": "kotoki_c_total",
	} {
		if got, ok := Resolve(tok, known); !ok || got != want {
			t.Errorf("Resolve(%s) = %q,%v want %q", tok, got, ok, want)
		}
	}
	// _total is never stripped: a typo'd counter must not resolve to a different metric.
	for _, tok := range []string{"kotoki_c", "kotoki_h_seconds_total", "kotoki_nope_bucket", "kotoki_c_total_bucket"} {
		if got, ok := Resolve(tok, known); ok {
			t.Errorf("Resolve(%s) = %q, want unknown", tok, got)
		}
	}
}

func TestLoadAllowed(t *testing.T) {
	dir := t.TempDir()
	write := func(s string) string {
		p := filepath.Join(dir, "m.txt")
		if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	got, err := LoadAllowed(write("# c\n\nkotoki_a_total\n?kotoki_b_total\n"))
	if err != nil || len(got) != 2 || got["kotoki_a_total"] || !got["kotoki_b_total"] {
		t.Fatalf("LoadAllowed = %v, %v", got, err)
	}
	for _, bad := range []string{"", "# only comment\n", "kotoki_a\nkotoki_a\n", "Kotoki_A\n", "foo_bar\n"} {
		if _, err := LoadAllowed(write(bad)); err == nil {
			t.Errorf("LoadAllowed(%q) = nil error", bad)
		}
	}
}

func TestRegisteredInCode_Forms(t *testing.T) {
	got, err := RegisteredInCode("testdata/ok")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"kotoki_cmd_summary", "kotoki_hist_seconds", "kotoki_lit_total", "kotoki_param_gauge",
		"kotoki_realtime_const_lit", "kotoki_realtime_helper_total", "kotoki_realtime_helper_two_total",
	}
	if g := Sorted(got); !reflect.DeepEqual(g, want) {
		t.Fatalf("RegisteredInCode = %v, want %v", g, want)
	}
}

func TestRegisteredInCode_UnresolvableNameFails(t *testing.T) {
	if _, err := RegisteredInCode("testdata/bad"); err == nil || !strings.Contains(err.Error(), "cannot resolve") {
		t.Fatalf("err = %v, want 'cannot resolve'", err)
	}
	if _, err := RegisteredInCode(t.TempDir()); err == nil {
		t.Fatal("empty tree must fail, not return an empty registry")
	}
}

func TestRegisteredInCode_RealTree(t *testing.T) {
	got, err := RegisteredInCode(repoRoot)
	if err != nil {
		t.Fatal(err)
	}
	// one metric per registration form used in the repo: literal, prefix helper, parameter helper
	for _, n := range []string{"kotoki_http_requests_total", "kotoki_realtime_sessions_active", "kotoki_realtime_turn_end_to_first_audio_seconds", "kotoki_db_pool_max_conns"} {
		if !got[n] {
			t.Errorf("%s (%s) not found in the code registry", n, owner(n))
		}
	}
}

// TestMetricsFileMatchesCode: metrics.txt is the canonical list; it must equal what the code
// registers, in both directions, so neither side can drift silently.
func TestMetricsFileMatchesCode(t *testing.T) {
	code, err := RegisteredInCode(repoRoot)
	if err != nil {
		t.Fatal(err)
	}
	allowed, err := LoadAllowed(metricsPath)
	if err != nil {
		t.Fatal(err)
	}
	for n := range allowed {
		if !code[n] {
			t.Errorf("metrics.txt lists %s but the code does not register it (owner %s)", n, owner(n))
		}
	}
	for n := range code {
		if _, ok := allowed[n]; !ok {
			t.Errorf("the code registers %s (owner %s) but metrics.txt does not list it; add it (go run ./test/tools/metricref/cmd/metricref)", n, owner(n))
		}
	}
}

// TestMetricsRegisteredInCode: every metric an alert uses is registered in code, whatever
// metrics.txt says (an alert on a metric nobody exports never fires).
func TestMetricsRegisteredInCode(t *testing.T) {
	code, err := RegisteredInCode(repoRoot)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range loadRules(t) {
		for _, tok := range Extract(r.Expr) {
			if _, ok := Resolve(tok, code); !ok {
				t.Errorf("alert %s uses %s, not registered in code (owner %s)", r.Alert, tok, owner(tok))
			}
		}
	}
}

func TestAlertsUseKnownMetrics(t *testing.T) {
	allowed, err := LoadAllowed(metricsPath)
	if err != nil {
		t.Fatal(err)
	}
	known := map[string]bool{}
	for n := range allowed {
		known[n] = true
	}
	for _, r := range loadRules(t) {
		if len(Extract(r.Expr)) == 0 && !regexp.MustCompile(`pg_up|redis_up|rabbitmq_|kube_|certmanager_|kotoki:`).MatchString(r.Expr) {
			t.Errorf("alert %s uses no metric at all", r.Alert)
		}
		for _, tok := range Extract(r.Expr) {
			if _, ok := Resolve(tok, known); !ok {
				t.Errorf("alert %s uses unknown metric %s (not in metrics.txt)", r.Alert, tok)
			}
		}
	}
}

// recordingRules are metrics-from-managed-services that Task 11 defines per environment.
var recordingRules = map[string]bool{
	"kotoki:postgres_cpu_utilization:ratio": true,
	"kotoki:redis_memory_utilization:ratio": true,
}

func TestAlertsUseDefinedRecordingRules(t *testing.T) {
	re := regexp.MustCompile(`kotoki:[a-z0-9_:]+`)
	used := map[string]bool{}
	for _, r := range loadRules(t) {
		for _, m := range re.FindAllString(r.Expr, -1) {
			used[m] = true
			if !recordingRules[m] {
				t.Errorf("alert %s uses recording rule %s that is not declared in recordingRules", r.Alert, m)
			}
		}
	}
	for m := range recordingRules {
		if !used[m] {
			t.Errorf("recording rule %s declared but no alert uses it", m)
		}
	}
}

// TestAlertLabelsFromPodMonitor: label app_kubernetes_io_component exists on scraped series only
// through the PodMonitor's podTargetLabels (Task 7).
func TestAlertLabelsFromPodMonitor(t *testing.T) {
	b, err := os.ReadFile(monitorPath)
	if err != nil {
		t.Fatal(err)
	}
	var pm struct {
		Spec struct {
			PodTargetLabels []string `yaml:"podTargetLabels"`
		} `yaml:"spec"`
	}
	if err := yaml.Unmarshal(b, &pm); err != nil {
		t.Fatal(err)
	}
	have := false
	for _, l := range pm.Spec.PodTargetLabels {
		have = have || l == "app.kubernetes.io/component"
	}
	for _, r := range loadRules(t) {
		if strings.Contains(r.Expr, "app_kubernetes_io_component") && !have {
			t.Errorf("alert %s groups by app_kubernetes_io_component but the PodMonitor does not copy that pod label", r.Alert)
		}
	}
}

func TestAlertNamesMatchSpec(t *testing.T) {
	want := readLines(t, namesPath)
	if len(want) != 34 {
		t.Fatalf("alert-names.txt has %d names, want 34", len(want))
	}
	seen := map[string]bool{}
	for _, n := range want {
		if seen[n] {
			t.Errorf("alert-names.txt: duplicate %s", n)
		}
		seen[n] = true
	}
	var got []string
	for _, r := range loadRules(t) {
		if r.Alert == "" {
			t.Errorf("a non-alert rule (record %q) in prometheusrule.yaml", r.Expr)
		}
		got = append(got, r.Alert)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("alert names/order differ from alert-names.txt (spec §13.7.5):\n got  %v\n want %v", got, want)
	}
}

// specTable is spec §13.7.5: severity and for per alert, and the runbook mapping of the plan.
var specTable = map[string]struct{ severity, forDur, runbook string }{
	"TurnLatencyP95High":        {"warning", "10m", "R1"},
	"TurnLatencyP95Critical":    {"critical", "5m", "R1"},
	"ServerOverheadHigh":        {"warning", "10m", "R1"},
	"BargeInStopP95High":        {"warning", "10m", "R1"},
	"RealtimeCapacityHigh":      {"warning", "5m", "R6"},
	"RealtimeRejectingCapacity": {"critical", "5m", "R6"},
	"RealtimeOutboundDrops":     {"warning", "10m", "R6"},
	"WSConnectFailureHigh":      {"warning", "10m", "R1"},
	"ApiErrorRateHigh":          {"critical", "5m", "R1"},
	"ApiLatencyP95High":         {"warning", "10m", "R1"},
	"ProviderErrorRateHigh":     {"warning", "5m", "R2"},
	"ProviderCircuitOpen":       {"warning", "2m", "R2"},
	"ProviderUnavailable":       {"critical", "1m", "R2"},
	"ProviderFallbackRateHigh":  {"warning", "15m", "R2"},
	"OutboxLagHigh":             {"warning", "5m", "R3"},
	"OutboxLagCritical":         {"critical", "5m", "R3"},
	"DLQNotEmpty":               {"warning", "10m", "R3"},
	"DLQGrowing":                {"critical", "5m", "R3"},
	"QueueBacklog":              {"warning", "15m", "R3"},
	"OutboxFailed":              {"warning", "5m", "R3"},
	"SweeperStale":              {"critical", "2m", "R9"},
	"SweeperHeartbeatSuspended": {"warning", "10m", "R9"},
	"CleanupStale":              {"warning", "10m", "R9"},
	"BalanceInvariantViolation": {"critical", "0m", "R7"},
	"PartitionsRunningOut":      {"critical", "30m", "R9"},
	"DBPoolSaturated":           {"warning", "5m", "R5"},
	"PostgresDown":              {"critical", "1m", "R5"},
	"PostgresCPUHigh":           {"warning", "15m", "R5"},
	"RedisDown":                 {"critical", "1m", "R4"},
	"RedisMemoryHigh":           {"warning", "10m", "R4"},
	"RabbitMQNodeDown":          {"warning", "2m", "R3"},
	"RabbitMQAlarm":             {"critical", "1m", "R3"},
	"PodCrashLooping":           {"warning", "0m", "R6"},
	"CertificateExpiringSoon":   {"warning", "1h", "R11"},
}

func TestAlertsMatchSpecTable(t *testing.T) {
	rules := loadRules(t)
	if len(specTable) != len(rules) {
		t.Fatalf("specTable has %d alerts, prometheusrule.yaml %d", len(specTable), len(rules))
	}
	for _, r := range rules {
		want, ok := specTable[r.Alert]
		if !ok {
			t.Errorf("alert %s not in spec §13.7.5", r.Alert)
			continue
		}
		if got := r.Labels["severity"]; got != want.severity {
			t.Errorf("%s severity = %q, want %q", r.Alert, got, want.severity)
		}
		if r.For != want.forDur {
			t.Errorf("%s for = %q, want %q", r.Alert, r.For, want.forDur)
		}
		if len(r.Labels) != 1 {
			t.Errorf("%s labels = %v, want only severity", r.Alert, r.Labels)
		}
		if strings.TrimSpace(r.Annotations["summary"]) == "" {
			t.Errorf("%s has no summary", r.Alert)
		}
		if strings.TrimSpace(r.Expr) == "" {
			t.Errorf("%s has no expr", r.Alert)
		}
	}
}

// runbookCheckFiles reports whether docs/runbook/R<n>-*.md must exist. RUNBOOK_CHECK=off skips,
// =on forces; unset arms itself as soon as docs/runbook exists (Task 23 creates the runbooks).
func runbookCheckFiles(env string, dirExists bool) bool {
	switch env {
	case "off":
		return false
	case "on":
		return true
	default:
		return dirExists
	}
}

func TestRunbookCheckFiles(t *testing.T) {
	for _, c := range []struct {
		env string
		dir bool
		out bool
	}{{"", false, false}, {"", true, true}, {"off", true, false}, {"on", false, true}, {"on", true, true}} {
		if got := runbookCheckFiles(c.env, c.dir); got != c.out {
			t.Errorf("runbookCheckFiles(%q,%v) = %v", c.env, c.dir, got)
		}
	}
}

// runbookSlug is the file-name convention Task 23 must follow: docs/runbook/R<n>-<slug>.md.
var runbookSlug = map[string]string{
	"R1": "turn-latency", "R2": "provider-degraded", "R3": "async-pipeline", "R4": "redis", "R5": "postgres",
	"R6": "realtime-capacity", "R7": "balance-invariant", "R8": "secret-leak", "R9": "jobs-stale",
	"R10": "vendor-switch", "R11": "gateway-certificate",
}

func TestAlertsHaveRunbook(t *testing.T) {
	_, dirErr := os.Stat(runbookDir)
	checkFiles := runbookCheckFiles(os.Getenv("RUNBOOK_CHECK"), dirErr == nil)
	re := regexp.MustCompile(`^R([0-9]+)$`)
	for _, r := range loadRules(t) {
		rb := r.Annotations["runbook"]
		m := re.FindStringSubmatch(rb)
		if m == nil {
			t.Errorf("%s: annotation runbook = %q, want R<n>", r.Alert, rb)
			continue
		}
		slug, ok := runbookSlug[rb]
		if !ok {
			t.Errorf("%s: runbook %s is outside R1..R11", r.Alert, rb)
			continue
		}
		if want := specTable[r.Alert].runbook; want != rb {
			t.Errorf("%s: runbook = %s, plan maps it to %s", r.Alert, rb, want)
		}
		if want := runbookBase + rb + "-" + slug + ".md"; r.Annotations["runbook_url"] != want {
			t.Errorf("%s: runbook_url = %q, want %q", r.Alert, r.Annotations["runbook_url"], want)
		}
		if checkFiles {
			if _, err := os.Stat(filepath.Join(runbookDir, rb+"-"+slug+".md")); err != nil {
				t.Errorf("%s: docs/runbook/%s-%s.md does not exist (Task 23; RUNBOOK_CHECK=off skips until then)", r.Alert, rb, slug)
			}
		}
	}
}

func TestSortedHelper(t *testing.T) {
	if got := Sorted(map[string]bool{"b": true, "a": true}); !sort.StringsAreSorted(got) || len(got) != 2 {
		t.Fatalf("Sorted = %v", got)
	}
}
