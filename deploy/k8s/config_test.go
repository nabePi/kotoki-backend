//go:build k8s

// Package k8s renders every kustomize overlay and feeds the environment of each application
// Deployment (ConfigMap data + literal env + dummy values for the keys of its Secret) to the real
// config.LoadAPI/LoadRealtime/LoadWorker and provider.RouterConfig.Validate (M10 Task 11, spec 13.2.13).
//
// Load* reads os.Environ. The shell of the developer holds real credentials, so nothing is ever set in
// or read from this process: each load runs in a helper subprocess (this test binary re-executed) whose
// environment is built ONLY from the rendered manifests. Messages carry variable names, never values.
package k8s

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/caarlos0/env/v11"

	"github.com/nabePi/kotoki-backend/internal/platform/config"
	"github.com/nabePi/kotoki-backend/internal/provider"
)

const helperEnv = "KOTOKI_K8S_LOAD_HELPER"

var overlays = []string{"dev", "staging", "prod"}

// ---------------------------------------------------------------- helper subprocess

type loadResult struct {
	Errors string `json:"errors"`
}

// TestHelperLoad is not a test: it is the body of the subprocess started by loadWith.
func TestHelperLoad(t *testing.T) {
	bin := os.Getenv(helperEnv)
	if bin == "" {
		t.Skip("helper process only")
	}
	var errs []error
	switch bin {
	case "api":
		_, err := config.LoadAPI()
		errs = append(errs, err)
	case "realtime":
		_, err := config.LoadRealtime()
		errs = append(errs, err)
		errs = append(errs, validateProviders([]provider.Capability{provider.CapSTT, provider.CapLLM, provider.CapTTS, provider.CapEmbedding}))
	case "worker":
		_, err := config.LoadWorker()
		errs = append(errs, err)
		errs = append(errs, validateProviders([]provider.Capability{provider.CapLLM, provider.CapEmbedding}))
	default:
		t.Fatalf("unknown helper binary %q", bin)
	}
	var msgs []string
	for _, err := range errs {
		if err != nil {
			msgs = append(msgs, err.Error())
		}
	}
	b, _ := json.Marshal(loadResult{Errors: strings.Join(msgs, "\n")})
	fmt.Printf("\nKOTOKI_RESULT %s\n", b)
	os.Exit(0)
}

// validateProviders mirrors cmd/internal/providerwire.ValidateReal (not importable from deploy/): every
// capability the process does not use is forced to fake, EVALUATION follows LLM, then
// RouterConfig.Validate plus the CARTESIA_VERSION check run.
func validateProviders(caps []provider.Capability) error {
	var rc provider.RouterConfig
	if err := env.Parse(&rc); err != nil {
		return fmt.Errorf("provider config: %w", err)
	}
	if slices.Contains(caps, provider.CapLLM) {
		caps = append(slices.Clone(caps), provider.CapEvaluation)
	}
	mode := func(c provider.Capability, dst *string) {
		if slices.Contains(caps, c) {
			*dst = rc.ModeFor(c)
		} else {
			*dst = provider.ModeFake
		}
	}
	e := rc
	mode(provider.CapSTT, &e.ModeSTT)
	mode(provider.CapLLM, &e.ModeLLM)
	mode(provider.CapEvaluation, &e.ModeEvaluation)
	mode(provider.CapTTS, &e.ModeTTS)
	mode(provider.CapEmbedding, &e.ModeEmbedding)
	errs := []error{e.Validate()}
	cartesia := (e.ModeFor(provider.CapSTT) == provider.ModeReal && e.CartesiaSTTEnabled) ||
		(e.ModeFor(provider.CapTTS) == provider.ModeReal && e.CartesiaTTSEnabled)
	if cartesia && e.CartesiaVersion == "" {
		errs = append(errs, fmt.Errorf("CARTESIA_VERSION must not be empty while a Cartesia capability is enabled in real mode"))
	}
	var msgs []string
	for _, err := range errs {
		if err != nil {
			msgs = append(msgs, err.Error())
		}
	}
	if len(msgs) == 0 {
		return nil
	}
	return fmt.Errorf("%s", strings.Join(msgs, "\n"))
}

// loadWith runs config.Load<bin> (+ provider validation) in a subprocess whose environment is exactly
// vars. It returns the validation error text ("" = the configuration is accepted).
func loadWith(t *testing.T, bin string, vars map[string]string) string {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperLoad$", "-test.count=1")
	cmd.Env = []string{helperEnv + "=" + bin}
	keys := slices.Sorted(maps.Keys(vars))
	for _, k := range keys {
		cmd.Env = append(cmd.Env, k+"="+vars[k])
	}
	var out, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("helper %s: %v\n%s", bin, err, stderr.String())
	}
	for _, line := range strings.Split(out.String(), "\n") {
		if rest, ok := strings.CutPrefix(line, "KOTOKI_RESULT "); ok {
			var r loadResult
			if err := json.Unmarshal([]byte(rest), &r); err != nil {
				t.Fatalf("helper %s: bad result: %v", bin, err)
			}
			return r.Errors
		}
	}
	t.Fatalf("helper %s: no result in output:\n%s", bin, out.String())
	return ""
}

// ---------------------------------------------------------------- render

type metadata struct {
	Name        string            `json:"name"`
	Namespace   string            `json:"namespace"`
	Annotations map[string]string `json:"annotations"`
}

type envVar struct {
	Name      string `json:"name"`
	Value     string `json:"value"`
	ValueFrom any    `json:"valueFrom"`
}

type envFromSrc struct {
	ConfigMapRef *struct {
		Name string `json:"name"`
	} `json:"configMapRef"`
	SecretRef *struct {
		Name string `json:"name"`
	} `json:"secretRef"`
}

type container struct {
	Name    string       `json:"name"`
	Env     []envVar     `json:"env"`
	EnvFrom []envFromSrc `json:"envFrom"`
}

type object struct {
	Kind     string            `json:"kind"`
	Metadata metadata          `json:"metadata"`
	Data     map[string]string `json:"data"` // ConfigMap and Secret
	Spec     struct {
		Target struct {
			Name string `json:"name"`
		} `json:"target"`
		SecretData []struct {
			SecretKey string `json:"secretKey"`
		} `json:"data"` // ExternalSecret
		Template struct {
			Spec struct {
				Containers []container `json:"containers"`
			} `json:"spec"`
		} `json:"template"` // Deployment
	} `json:"spec"`
}

type render struct {
	name string
	objs []object
}

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found")
		}
		dir = parent
	}
}

var renders = map[string]*render{}

func renderOverlay(t *testing.T, name string) *render {
	t.Helper()
	if r, ok := renders[name]; ok {
		return r
	}
	root := repoRoot(t)
	kustomize, yq := filepath.Join(root, "bin/tools/kustomize"), filepath.Join(root, "bin/tools/yq")
	for _, tool := range []string{kustomize, yq} {
		if _, err := os.Stat(tool); err != nil {
			t.Skipf("%s tidak ada; jalankan 'make k8s-tools'", tool)
		}
	}
	var yamlOut, stderr bytes.Buffer
	build := exec.Command(kustomize, "build", filepath.Join(root, "deploy/k8s/overlays", name))
	build.Stdout, build.Stderr = &yamlOut, &stderr
	if err := build.Run(); err != nil {
		t.Fatalf("kustomize build overlays/%s: %v\n%s", name, err, stderr.String())
	}
	var jsonOut bytes.Buffer
	conv := exec.Command(yq, "ea", "-o=json", "[.]", "-")
	conv.Stdin, conv.Stdout, conv.Stderr = &yamlOut, &jsonOut, &stderr
	if err := conv.Run(); err != nil {
		t.Fatalf("yq overlays/%s: %v\n%s", name, err, stderr.String())
	}
	r := &render{name: name}
	if err := json.Unmarshal(jsonOut.Bytes(), &r.objs); err != nil {
		t.Fatalf("overlays/%s: %v", name, err)
	}
	if len(r.objs) == 0 {
		t.Fatalf("overlays/%s: render kosong", name)
	}
	renders[name] = r
	return r
}

func (r *render) find(kind, name string) *object {
	for i := range r.objs {
		if r.objs[i].Kind == kind && r.objs[i].Metadata.Name == name {
			return &r.objs[i]
		}
	}
	return nil
}

// appDeployments are the Deployments that run a Kotoki binary and the config loader of each.
var appDeployments = []struct{ name, bin string }{
	{"kotoki-api", "api"}, {"kotoki-realtime", "realtime"},
	{"kotoki-worker-core", "worker"}, {"kotoki-worker-evaluation", "worker"},
	{"kotoki-worker-memory", "worker"}, {"kotoki-worker-learner", "worker"},
}

// secretKeys returns the keys of the Secret a workload mounts: the data keys of a rendered Secret (dev) or
// the secretKeys of the ExternalSecret that produces it (staging, prod). Never values.
func (r *render) secretKeys(t *testing.T, secret string) []string {
	t.Helper()
	var keys []string
	if s := r.find("Secret", secret); s != nil {
		keys = slices.Sorted(maps.Keys(s.Data))
	}
	for i := range r.objs {
		o := &r.objs[i]
		if o.Kind == "ExternalSecret" && o.Spec.Target.Name == secret {
			for _, d := range o.Spec.SecretData {
				keys = append(keys, d.SecretKey)
			}
		}
	}
	if len(keys) == 0 {
		t.Fatalf("%s: tidak ada Secret/ExternalSecret untuk %s", r.name, secret)
	}
	sort.Strings(keys)
	return keys
}

// dummyValue is a syntactically valid stand-in for a Secret key. Dev accepts non-TLS dependencies.
func dummyValue(t *testing.T, overlay, key string, jwt string) (string, bool) {
	t.Helper()
	dev := overlay == "dev"
	switch key {
	case "DB_URL", "DB_MIGRATE_URL":
		if dev {
			return "postgres://u:p@h:5432/kotoki?sslmode=disable", true
		}
		return "postgres://u:p@h:5432/kotoki?sslmode=verify-full&sslrootcert=/etc/kotoki/pg-ca.pem", true
	case "REDIS_URL":
		if dev {
			return "redis://h:6379/0", true
		}
		return "rediss://:p@h:6380/0", true
	case "RABBITMQ_URL":
		if dev {
			return "amqp://u:p@h:5672/%2Fkotoki", true
		}
		return "amqps://u:p@h:5671/%2Fkotoki", true
	case "AUTH_JWT_SECRET":
		return jwt, true
	case "AUTH_JWT_PREVIOUS_SECRET":
		return "", false // not rotating: unset
	case "SMTP_PASSWORD":
		return "dummy-smtp-password", true
	case "OPENAI_API_KEY", "DEEPSEEK_API_KEY", "ELEVENLABS_API_KEY", "CARTESIA_API_KEY":
		return "dummy-" + strings.ToLower(strings.TrimSuffix(key, "_API_KEY")) + "-key", true
	case "host": // kotoki-keda-secrets: not read by any binary
		return "", false
	}
	t.Fatalf("%s: kunci Secret tak dikenal %q (tambahkan nilai dummy bila memang sah)", overlay, key)
	return "", false
}

func randomJWT(t *testing.T) string {
	t.Helper()
	b := make([]byte, 48)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(b)
}

// workloadEnv assembles the environment of a Deployment the way the kubelet does: envFrom in order (later
// wins), then literal env; Secret keys get dummy values.
func (r *render) workloadEnv(t *testing.T, deployment string) map[string]string {
	t.Helper()
	d := r.find("Deployment", deployment)
	if d == nil {
		t.Fatalf("%s: Deployment %s tidak ada", r.name, deployment)
	}
	if len(d.Spec.Template.Spec.Containers) == 0 {
		t.Fatalf("%s: %s tanpa container", r.name, deployment)
	}
	c := d.Spec.Template.Spec.Containers[0]
	out := map[string]string{}
	jwt := randomJWT(t)
	for _, ef := range c.EnvFrom {
		switch {
		case ef.ConfigMapRef != nil:
			cm := r.find("ConfigMap", ef.ConfigMapRef.Name)
			if cm == nil {
				t.Fatalf("%s: %s merujuk ConfigMap %s yang tidak dirender", r.name, deployment, ef.ConfigMapRef.Name)
			}
			maps.Copy(out, cm.Data)
		case ef.SecretRef != nil:
			for _, k := range r.secretKeys(t, ef.SecretRef.Name) {
				if v, ok := dummyValue(t, r.name, k, jwt); ok {
					out[k] = v
				}
			}
		}
	}
	for _, e := range c.Env {
		if e.ValueFrom != nil {
			t.Fatalf("%s: %s env %s memakai valueFrom (tidak diharapkan)", r.name, deployment, e.Name)
		}
		out[e.Name] = e.Value
	}
	// empty values are "unset" for the loader (ConfigMap key AUTH_REFRESH_COOKIE_DOMAIN: "")
	for k, v := range out {
		if v == "" {
			delete(out, k)
		}
	}
	return out
}

func clone(m map[string]string) map[string]string { return maps.Clone(m) }

// ---------------------------------------------------------------- tests

// TestOverlaysPassConfigValidate: the legitimate overlays boot every binary (Validate() == nil).
func TestOverlaysPassConfigValidate(t *testing.T) {
	for _, ov := range overlays {
		r := renderOverlay(t, ov)
		for _, d := range appDeployments {
			t.Run(ov+"/"+d.name, func(t *testing.T) {
				if got := loadWith(t, d.bin, r.workloadEnv(t, d.name)); got != "" {
					t.Fatalf("konfigurasi %s/%s ditolak:\n%s", ov, d.name, got)
				}
			})
		}
	}
}

// TestProdRejectsForbiddenConfig: the settings forbidden in prod (spec 13.2.13) make the binary refuse to
// start; staging refuses the same set except PROVIDER_MODE=fake (load tests).
func TestProdRejectsForbiddenConfig(t *testing.T) {
	type mutation struct {
		name    string
		deploy  string
		set     map[string]string
		want    string // substring of the error (a variable name)
		inStage bool   // also refused in staging
	}
	muts := []mutation{
		{"PROVIDER_MODE=fake api", "kotoki-api", map[string]string{"PROVIDER_MODE": "fake"}, "PROVIDER_MODE", false},
		{"PROVIDER_MODE=fake realtime", "kotoki-realtime", map[string]string{"PROVIDER_MODE": "fake"}, "PROVIDER_MODE", false},
		{"PROVIDER_MODE=fake worker", "kotoki-worker-evaluation", map[string]string{"PROVIDER_MODE": "fake"}, "PROVIDER_MODE", false},
		{"PROVIDER_MODE_TTS=fake realtime", "kotoki-realtime", map[string]string{"PROVIDER_MODE_TTS": "fake"}, "PROVIDER_MODE_TTS", false},
		{"PROVIDER_MODE_LLM=fake worker", "kotoki-worker-memory", map[string]string{"PROVIDER_MODE_LLM": "fake"}, "PROVIDER_MODE_LLM", false},
		{"CORS origin http", "kotoki-api", map[string]string{"CORS_ALLOWED_ORIGINS": "http://app.kotoki.id"}, "CORS_ALLOWED_ORIGINS", true},
		{"CORS origin wildcard", "kotoki-api", map[string]string{"CORS_ALLOWED_ORIGINS": "*"}, "CORS_ALLOWED_ORIGINS", true},
		{"WS origin http", "kotoki-realtime", map[string]string{"WS_ALLOWED_ORIGINS": "http://app.kotoki.id"}, "WS_ALLOWED_ORIGINS", true},
		{"WS empty origin", "kotoki-realtime", map[string]string{"WS_ALLOW_EMPTY_ORIGIN": "true"}, "WS_ALLOW_EMPTY_ORIGIN", true},
		{"LOG_TRANSCRIPTS api", "kotoki-api", map[string]string{"LOG_TRANSCRIPTS": "true"}, "LOG_TRANSCRIPTS", true},
		{"LOG_TRANSCRIPTS realtime", "kotoki-realtime", map[string]string{"LOG_TRANSCRIPTS": "true"}, "LOG_TRANSCRIPTS", true},
		{"LOG_TRANSCRIPTS worker", "kotoki-worker-core", map[string]string{"LOG_TRANSCRIPTS": "true"}, "LOG_TRANSCRIPTS", true},
		{"LOG_FORMAT text", "kotoki-api", map[string]string{"LOG_FORMAT": "text"}, "LOG_FORMAT", true},
		{"MAIL_MODE=log", "kotoki-api", map[string]string{"MAIL_MODE": "log"}, "MAIL_MODE", true},
		{"cookie not secure", "kotoki-api", map[string]string{"AUTH_REFRESH_COOKIE_SECURE": "false"}, "AUTH_REFRESH_COOKIE_SECURE", true},
		{"reset URL http", "kotoki-api", map[string]string{"PASSWORD_RESET_URL_BASE": "http://app.kotoki.id/reset-password"}, "PASSWORD_RESET_URL_BASE", true},
		{"DB sslmode=disable", "kotoki-api", map[string]string{"DB_URL": "postgres://u:p@h:5432/kotoki?sslmode=disable"}, "DB_URL", true},
		{"Redis without TLS", "kotoki-realtime", map[string]string{"REDIS_URL": "redis://h:6379/0"}, "REDIS_URL", true},
		{"RabbitMQ without TLS", "kotoki-worker-core", map[string]string{"RABBITMQ_URL": "amqp://u:p@h:5672/%2Fkotoki"}, "RABBITMQ_URL", true},
		{"APP_ENV prod unknown", "kotoki-api", map[string]string{"APP_ENV": "qa"}, "APP_ENV", true},
		{"realtime ready > max", "kotoki-realtime", map[string]string{"REALTIME_READY_MAX_SESSIONS": "101"}, "REALTIME_READY_MAX_SESSIONS", true},
		{"JWT secret too short", "kotoki-api", map[string]string{"AUTH_JWT_SECRET": base64.StdEncoding.EncodeToString([]byte("short"))}, "AUTH_JWT_SECRET", true},
	}
	for _, ov := range []string{"prod", "staging"} {
		r := renderOverlay(t, ov)
		bins := map[string]string{}
		for _, d := range appDeployments {
			bins[d.name] = d.bin
		}
		for _, m := range muts {
			t.Run(ov+"/"+m.name, func(t *testing.T) {
				vars := clone(r.workloadEnv(t, m.deploy))
				maps.Copy(vars, m.set)
				got := loadWith(t, bins[m.deploy], vars)
				if ov == "staging" && !m.inStage {
					if got != "" {
						t.Fatalf("staging harus menerima %s (load test), got:\n%s", m.name, got)
					}
					return
				}
				if got == "" {
					t.Fatalf("%s: %s diterima padahal terlarang", ov, m.name)
				}
				if !strings.Contains(got, m.want) {
					t.Fatalf("%s: %s: pesan tidak menyebut %s:\n%s", ov, m.name, m.want, got)
				}
				for _, secretValue := range []string{"u:p@", ":p@h"} {
					if strings.Contains(got, secretValue) {
						t.Fatalf("%s: %s: pesan membocorkan kredensial", ov, m.name)
					}
				}
			})
		}
	}
}

// TestProviderSwitchesExactlyOne: spec 8.5.1/13.2.8 (D-017). Every overlay has exactly one STT and one LLM
// vendor switch on and at least one TTS switch; realtime and worker read the SAME ConfigMap for *_LLM_ENABLED;
// zero or two STT/LLM switches (or no TTS) make the binary refuse to start and the message names every switch.
func TestProviderSwitchesExactlyOne(t *testing.T) {
	stt := []string{"OPENAI_STT_ENABLED", "ELEVENLABS_STT_ENABLED", "CARTESIA_STT_ENABLED"}
	llm := []string{"OPENAI_LLM_ENABLED", "DEEPSEEK_LLM_ENABLED"}
	tts := []string{"ELEVENLABS_TTS_ENABLED", "CARTESIA_TTS_ENABLED"}
	count := func(m map[string]string, names []string) int {
		n := 0
		for _, k := range names {
			if m[k] == "true" {
				n++
			}
		}
		return n
	}
	for _, ov := range overlays {
		r := renderOverlay(t, ov)
		rt, wk := r.workloadEnv(t, "kotoki-realtime"), r.workloadEnv(t, "kotoki-worker-evaluation")
		if n := count(rt, stt); n != 1 {
			t.Errorf("%s: %d sakelar STT true, want 1", ov, n)
		}
		if n := count(rt, llm); n != 1 {
			t.Errorf("%s: %d sakelar LLM true, want 1", ov, n)
		}
		if n := count(rt, tts); n < 1 {
			t.Errorf("%s: tidak ada sakelar TTS true", ov)
		}
		for _, k := range llm {
			if rt[k] != wk[k] {
				t.Errorf("%s: %s berbeda antara realtime (%q) dan worker (%q)", ov, k, rt[k], wk[k])
			}
		}
		if ov == "dev" {
			continue // PROVIDER_MODE=fake: switches are not validated (spec 8.5.1)
		}
		cases := []struct {
			name string
			set  map[string]string
			want []string
		}{
			{"zero STT", map[string]string{"OPENAI_STT_ENABLED": "false", "ELEVENLABS_STT_ENABLED": "false", "CARTESIA_STT_ENABLED": "false"}, stt},
			{"two STT", map[string]string{"OPENAI_STT_ENABLED": "true", "CARTESIA_STT_ENABLED": "true"}, stt},
			{"zero LLM", map[string]string{"OPENAI_LLM_ENABLED": "false", "DEEPSEEK_LLM_ENABLED": "false"}, llm},
			{"two LLM", map[string]string{"OPENAI_LLM_ENABLED": "true", "DEEPSEEK_LLM_ENABLED": "true"}, llm},
			{"zero TTS", map[string]string{"ELEVENLABS_TTS_ENABLED": "false", "CARTESIA_TTS_ENABLED": "false"}, tts},
		}
		for _, c := range cases {
			t.Run(ov+"/"+c.name, func(t *testing.T) {
				vars := clone(rt)
				maps.Copy(vars, c.set)
				got := loadWith(t, "realtime", vars)
				if got == "" {
					t.Fatalf("%s diterima padahal sakelar salah", c.name)
				}
				for _, name := range c.want {
					if !strings.Contains(got, name) {
						t.Errorf("pesan tidak menyebut %s:\n%s", name, got)
					}
				}
			})
		}
		t.Run(ov+"/worker two LLM", func(t *testing.T) {
			vars := clone(wk)
			vars["DEEPSEEK_LLM_ENABLED"], vars["OPENAI_LLM_ENABLED"] = "true", "true"
			got := loadWith(t, "worker", vars)
			if got == "" || !strings.Contains(got, "OPENAI_LLM_ENABLED") || !strings.Contains(got, "DEEPSEEK_LLM_ENABLED") {
				t.Fatalf("worker harus menolak dua sakelar LLM dan menyebut keduanya, got:\n%s", got)
			}
		})
	}
}

// TestEnabledVendorHasSecretKey: a vendor enabled in the overlay has its API key in the Secret of every
// binary that needs it (spec 13.8.4): OpenAI always (embedding); DeepSeek when its LLM is on (realtime and
// worker); ElevenLabs/Cartesia when an STT/TTS capability of that vendor is on (realtime only).
func TestEnabledVendorHasSecretKey(t *testing.T) {
	for _, ov := range overlays {
		if ov == "dev" {
			continue // fake mode: Secret dev carries no vendor keys
		}
		r := renderOverlay(t, ov)
		pc := r.find("ConfigMap", "kotoki-provider-config")
		if pc == nil {
			t.Fatalf("%s: kotoki-provider-config tidak ada", ov)
		}
		on := func(k string) bool { return pc.Data[k] == "true" }
		has := func(deploy, key string) bool {
			c := r.find("Deployment", deploy).Spec.Template.Spec.Containers[0]
			for _, ef := range c.EnvFrom {
				if ef.SecretRef != nil && slices.Contains(r.secretKeys(t, ef.SecretRef.Name), key) {
					return true
				}
			}
			return false
		}
		need := []struct {
			deploy, key string
			required    bool
			why         string
		}{
			{"kotoki-realtime", "OPENAI_API_KEY", true, "embedding"},
			{"kotoki-worker-evaluation", "OPENAI_API_KEY", true, "embedding"},
			{"kotoki-realtime", "DEEPSEEK_API_KEY", on("DEEPSEEK_LLM_ENABLED"), "DEEPSEEK_LLM_ENABLED"},
			{"kotoki-worker-evaluation", "DEEPSEEK_API_KEY", on("DEEPSEEK_LLM_ENABLED"), "DEEPSEEK_LLM_ENABLED"},
			{"kotoki-realtime", "ELEVENLABS_API_KEY", on("ELEVENLABS_STT_ENABLED") || on("ELEVENLABS_TTS_ENABLED"), "ELEVENLABS_*_ENABLED"},
			{"kotoki-realtime", "CARTESIA_API_KEY", on("CARTESIA_STT_ENABLED") || on("CARTESIA_TTS_ENABLED"), "CARTESIA_*_ENABLED"},
		}
		for _, n := range need {
			if n.required && !has(n.deploy, n.key) {
				t.Errorf("%s: %s aktif (%s) tetapi %s tidak ada di Secret %s", ov, n.key, n.why, n.key, n.deploy)
			}
		}
		// least privilege: no ElevenLabs/Cartesia key outside realtime, and no key a binary never uses
		for _, d := range appDeployments {
			if d.name == "kotoki-realtime" {
				continue
			}
			for _, k := range []string{"ELEVENLABS_API_KEY", "CARTESIA_API_KEY"} {
				if has(d.name, k) {
					t.Errorf("%s: %s memuat %s (hanya boleh di realtime)", ov, d.name, k)
				}
			}
		}
		for _, k := range []string{"OPENAI_API_KEY", "DEEPSEEK_API_KEY"} {
			if has("kotoki-api", k) {
				t.Errorf("%s: kotoki-api memuat %s (api tanpa kunci vendor)", ov, k)
			}
		}
	}
}

var secretKeyRE = regexp.MustCompile(`(^|_)(URL|KEY|PASSWORD|SECRET)$|_API_KEY$|^host$`)

// nonSecretURLKeys are *_URL keys that are public endpoints and carry no credential.
var nonSecretURLKeys = regexp.MustCompile(`^([A-Z]+_)?(WS_)?BASE_URL$|^REALTIME_PUBLIC_URL$`)

var credentialURL = regexp.MustCompile(`://[^/@ ]+:[^/@ ]+@`)

// checkNoSecretsInConfigMaps returns the ConfigMap keys that look like secrets (name or credential URL).
func checkNoSecretsInConfigMaps(objs []object) []string {
	var bad []string
	for _, o := range objs {
		if o.Kind != "ConfigMap" {
			continue
		}
		for k, v := range o.Data {
			if (secretKeyRE.MatchString(k) && !nonSecretURLKeys.MatchString(k)) || credentialURL.MatchString(v) {
				bad = append(bad, o.Metadata.Name+"/"+k)
			}
		}
	}
	sort.Strings(bad)
	return bad
}

// TestNoSecretsInConfigMaps: no ConfigMap of any overlay carries a secret key or a credential URL, and
// the checker does catch an injected one (a secret smuggled into the ConfigMap of a binary).
func TestNoSecretsInConfigMaps(t *testing.T) {
	for _, ov := range overlays {
		r := renderOverlay(t, ov)
		if bad := checkNoSecretsInConfigMaps(r.objs); len(bad) > 0 {
			t.Errorf("%s: kunci mirip rahasia di ConfigMap: %v", ov, bad)
		}
		for _, inj := range []map[string]string{
			{"DB_URL": "postgres://u:p@h:5432/kotoki"}, {"OPENAI_API_KEY": "x"}, {"AUTH_JWT_SECRET": "x"},
			{"SMTP_PASSWORD": "x"}, {"RABBITMQ_URL": "amqps://u:p@h:5671/"}, {"SOMETHING": "postgres://u:p@h:5432/db"},
		} {
			mutated := append(slices.Clone(r.objs), object{Kind: "ConfigMap", Metadata: metadata{Name: "injected"}, Data: inj})
			if bad := checkNoSecretsInConfigMaps(mutated); len(bad) == 0 {
				t.Errorf("%s: rahasia di ConfigMap tidak terdeteksi: %v", ov, inj)
			}
		}
		// the vendor keys live only in the ExternalSecret/Secret of realtime and worker
		for _, o := range r.objs {
			if o.Kind == "ConfigMap" {
				for _, k := range []string{"CARTESIA_API_KEY", "ELEVENLABS_API_KEY", "OPENAI_API_KEY", "DEEPSEEK_API_KEY"} {
					if _, ok := o.Data[k]; ok {
						t.Errorf("%s: %s ada di ConfigMap %s", ov, k, o.Metadata.Name)
					}
				}
			}
		}
	}
}

var legacyEnv = regexp.MustCompile(`^PROVIDER_(PROFILE|TTS|STT|LLM)_|DEEPGRAM|GEMINI`)

// TestNoLegacyProviderEnv: D-015 (TTS per character), D-016 (no tiers), D-017 (vendor switches): none of the
// removed variable names and no Deepgram/Gemini key appears in any ConfigMap, Deployment env or ExternalSecret.
func TestNoLegacyProviderEnv(t *testing.T) {
	for _, ov := range overlays {
		r := renderOverlay(t, ov)
		for _, o := range r.objs {
			var names []string
			for k := range o.Data {
				names = append(names, k)
			}
			for _, d := range o.Spec.SecretData {
				names = append(names, d.SecretKey)
			}
			for _, c := range o.Spec.Template.Spec.Containers {
				for _, e := range c.Env {
					names = append(names, e.Name)
				}
			}
			for _, n := range names {
				// PROVIDER_TTS_FALLBACK_ENABLED is the live incident switch of provider.RouterConfig, not a legacy name.
				if legacyEnv.MatchString(n) && n != "PROVIDER_TTS_FALLBACK_ENABLED" {
					t.Errorf("%s: %s/%s memuat nama env legacy %s", ov, o.Kind, o.Metadata.Name, n)
				}
			}
		}
	}
}

// TestProdHasNoFake: prod and staging render PROVIDER_MODE=real and carry no FAKE_* knob.
func TestProdHasNoFake(t *testing.T) {
	for _, ov := range []string{"staging", "prod"} {
		r := renderOverlay(t, ov)
		for _, o := range r.objs {
			if o.Kind != "ConfigMap" {
				continue
			}
			for k, v := range o.Data {
				if strings.HasPrefix(k, "FAKE_") || (strings.HasPrefix(k, "PROVIDER_MODE") && v == "fake") {
					t.Errorf("%s: ConfigMap %s memuat %s=%s", ov, o.Metadata.Name, k, v)
				}
			}
		}
		pc := r.find("ConfigMap", "kotoki-provider-config")
		if pc == nil || pc.Data["PROVIDER_MODE"] != "real" {
			t.Errorf("%s: PROVIDER_MODE harus real", ov)
		}
	}
}
