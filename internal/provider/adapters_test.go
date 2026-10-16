package provider_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/parser"
	"go/token"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/provider"
	"github.com/nabePi/kotoki-backend/internal/provider/cartesia"
	"github.com/nabePi/kotoki-backend/internal/provider/deepseek"
	"github.com/nabePi/kotoki-backend/internal/provider/elevenlabs"
	"github.com/nabePi/kotoki-backend/internal/provider/httpx"
	"github.com/nabePi/kotoki-backend/internal/provider/openai"
	"github.com/nabePi/kotoki-backend/internal/testutil"
	"github.com/nabePi/kotoki-backend/internal/testutil/replay"
)

// vendors are the only vendor packages that may exist under internal/provider (D-017); the other
// directories are infrastructure, not adapters.
var (
	vendors     = []string{"openai", "deepseek", "elevenlabs", "cartesia"}
	nonVendors  = []string{"fake", "httpx", "pricing", "providertest"}
	secretKey   = "SECRETKEY-123"
	userSecret  = "ユーザー発話-SENTINEL-98765"
	sttSpeechMs = 1200
)

// ---- import rules ------------------------------------------------------------------------

// vendorImportViolations returns what is wrong with the import list of a vendor adapter
// (non-test files): only the standard library, coder/websocket, prometheus/client_golang,
// platform/otel, provider and provider/httpx are allowed (spec 8.1 item 2, depguard 14.9.5).
func vendorImportViolations(vendor string, imports []string) []string {
	allowedExact := map[string]bool{
		"github.com/coder/websocket": true, modPath + "internal/platform/otel": true,
		modPath + "internal/provider": true, modPath + "internal/provider/httpx": true,
	}
	var bad []string
	for _, p := range imports {
		switch {
		case isStdlib(p), allowedExact[p], p == "github.com/prometheus/client_golang" || strings.HasPrefix(p, "github.com/prometheus/client_golang/"):
		case strings.HasPrefix(p, modPath+"internal/conversation"):
			bad = append(bad, vendor+" imports "+p+" (conversation/audio and other feature packages are off limits)")
		case p == modPath+"internal/provider/fake" || p == modPath+"internal/provider/pricing":
			bad = append(bad, vendor+" imports "+p)
		case strings.HasPrefix(p, modPath+"internal/provider/"):
			bad = append(bad, vendor+" imports another provider package: "+p)
		default:
			bad = append(bad, vendor+" imports forbidden package "+p)
		}
	}
	return bad
}

func TestVendorImportViolationsDetector(t *testing.T) {
	good := []string{"context", "net/http", "github.com/coder/websocket", modPath + "internal/provider", modPath + "internal/provider/httpx"}
	require.Empty(t, vendorImportViolations("x", good))
	for _, p := range []string{
		modPath + "internal/conversation/audio", modPath + "internal/provider/fake", modPath + "internal/provider/pricing",
		modPath + "internal/provider/openai", modPath + "internal/provider/deepseek", modPath + "internal/postgres",
		modPath + "internal/provider/deepgram", "github.com/sashabaranov/go-openai", "gopkg.in/yaml.v3",
	} {
		require.NotEmpty(t, vendorImportViolations("x", append([]string{"context"}, p)), p)
	}
}

func TestAdaptersImportRules(t *testing.T) {
	entries, err := os.ReadDir(".")
	require.NoError(t, err)
	known := map[string]bool{}
	for _, v := range append(append([]string{}, vendors...), nonVendors...) {
		known[v] = true
	}
	for _, e := range entries {
		if e.IsDir() {
			require.True(t, known[e.Name()], "internal/provider/%s: only %v are vendors and %v infrastructure (no deepgram|gemini|azure|...)", e.Name(), vendors, nonVendors)
		}
	}
	for _, v := range vendors {
		st, err := os.Stat(v)
		require.NoError(t, err, "vendor package %s must exist", v)
		require.True(t, st.IsDir())
		var all []string
		files := 0
		for f, imps := range importsOf(t, v) {
			files++
			for _, p := range imps {
				all = append(all, p)
				require.NotContains(t, p, "deepgram", f)
				require.NotContains(t, p, "gemini", f)
			}
		}
		require.Positive(t, files, "no non-test files in %s", v)
		require.Empty(t, vendorImportViolations(v, all))
		for _, other := range vendors {
			if other != v {
				for _, p := range all {
					require.NotEqual(t, modPath+"internal/provider/"+other, p, "%s must not import %s", v, other)
				}
			}
		}
	}
}

// TestAdaptersImportOnlyInTestsOrWiring: nothing but the wiring package, cmd and tests may import a
// vendor package (the rest of the module depends on ports, §8.1).
func TestAdaptersImportOnlyInWiring(t *testing.T) {
	root := filepath.Join("..", "..")
	var offenders []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "vendor", "testdata", "bin":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		af, perr := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if perr != nil {
			return perr
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		if strings.HasPrefix(rel, "internal/provider/") || strings.HasPrefix(rel, "cmd/internal/providerwire/") {
			return nil
		}
		for _, im := range af.Imports {
			for _, v := range vendors {
				if strings.Trim(im.Path.Value, `"`) == modPath+"internal/provider/"+v {
					offenders = append(offenders, rel+" imports "+v)
				}
			}
		}
		return nil
	})
	require.NoError(t, err)
	require.Empty(t, offenders, "vendor adapters are assembled only in cmd/internal/providerwire")
}

// ---- adapter harness ---------------------------------------------------------------------

type proto int

const (
	protoHTTP proto = iota
	protoWS
)

// adapterCase is one (vendor, capability) pair. exercise runs one use of the adapter against url
// with log, and returns every *provider.Error (or error) it saw. With abort it cancels as soon as
// data flows (or shortly after the call started for non-streaming calls).
type adapterCase struct {
	vendor, capab string
	proto         proto
	oldKey        string // the key the vendor fixtures use in header expectations
	httpPath      string // path of the inline HTTP error scripts
	errCode       string // vendor error code of the WS error frame in the secret test
	ok            string // fixture of a normal run
	abortFixture  string // fixture of the abort run; empty = hang script
	exercise      func(t *testing.T, url string, log *slog.Logger, abort bool) []error
}

func speechPCM() []byte {
	n := 16 * sttSpeechMs
	pcm := make([]byte, n*2)
	for i := 0; i < n; i++ {
		v := int16(8000 * math.Sin(2*math.Pi*220*float64(i)/16000))
		pcm[2*i], pcm[2*i+1] = byte(v), byte(v>>8)
	}
	return pcm
}

func llmCase(vendor, oldKey string, mk func(url string, log *slog.Logger) (provider.LLMProvider, error)) adapterCase {
	return adapterCase{vendor: vendor, capab: "LLM", proto: protoHTTP, oldKey: oldKey, httpPath: "/chat/completions", ok: "llm_suite", abortFixture: "llm_suite_slow",
		exercise: func(t *testing.T, url string, log *slog.Logger, abort bool) []error {
			l, err := mk(url, log)
			require.NoError(t, err)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			s, err := l.Stream(ctx, provider.LLMRequest{Model: "m1", MaxTokens: 50, RequestID: "r1",
				Messages: []provider.Message{{Role: provider.RoleSystem, Content: "S"}, {Role: provider.RoleUser, Content: userSecret}}})
			if err != nil {
				return []error{err}
			}
			defer func() { _ = s.Close() }()
			var errs []error
			timeout := time.After(5 * time.Second)
			for {
				select {
				case ev, ok := <-s.Events():
					if !ok {
						return errs
					}
					if ev.Err != nil {
						errs = append(errs, ev.Err)
					}
					if abort && ev.Type == provider.LLMDelta {
						cancel()
					}
				case <-timeout:
					t.Fatal("LLM stream did not end")
				}
			}
		}}
}

func evalCase(vendor, oldKey string, mk func(url string, log *slog.Logger) (provider.EvaluationProvider, error)) adapterCase {
	return adapterCase{vendor: vendor, capab: "EVALUATION", proto: protoHTTP, oldKey: oldKey, httpPath: "/chat/completions", ok: "eval_suite",
		exercise: func(t *testing.T, url string, log *slog.Logger, abort bool) []error {
			p, err := mk(url, log)
			require.NoError(t, err)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if abort {
				time.AfterFunc(20*time.Millisecond, cancel)
			}
			_, err = p.GenerateJSON(ctx, provider.StructuredRequest{Task: "EVALUATION", Model: "m", SchemaName: "kotoki_evaluation_rubric_v1",
				JSONSchema: json.RawMessage(`{"type":"object","properties":{"s":{"type":"integer"}},"required":["s"],"additionalProperties":false}`),
				Messages:   []provider.Message{{Role: provider.RoleSystem, Content: "S"}, {Role: provider.RoleUser, Content: userSecret}}, MaxOutputTokens: 100})
			if err != nil {
				return []error{err}
			}
			return nil
		}}
}

func embedCase() adapterCase {
	return adapterCase{vendor: "openai", capab: "EMBEDDING", proto: protoHTTP, oldKey: "sk-test", httpPath: "/embeddings", ok: "embed_suite",
		exercise: func(t *testing.T, url string, log *slog.Logger, abort bool) []error {
			p, err := openai.NewEmbedding(openai.Config{APIKey: secretKey, BaseURL: url}, httpx.NewClient(), log)
			require.NoError(t, err)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if abort {
				time.AfterFunc(20*time.Millisecond, cancel)
			}
			_, err = p.Embed(ctx, provider.EmbeddingRequest{Texts: []string{userSecret, "b", "c"}, Model: "m", Dimensions: 1536})
			if err != nil {
				return []error{err}
			}
			return nil
		}}
}

func sttExercise(open func(url string, log *slog.Logger) (provider.STTProvider, error)) func(t *testing.T, url string, log *slog.Logger, abort bool) []error {
	return func(t *testing.T, url string, log *slog.Logger, abort bool) []error {
		p, err := open(url, log)
		require.NoError(t, err)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		s, err := p.Open(ctx, provider.STTSessionConfig{Language: "ja", SampleRate: provider.STTSampleRate, Keyterms: []string{userSecret}})
		if err != nil {
			return []error{err}
		}
		defer func() { _ = s.Close() }()
		pcm := speechPCM()
		for off := 0; off < len(pcm); off += 640 {
			if err := s.SendAudio(pcm[off:min(off+640, len(pcm))]); err != nil {
				time.Sleep(time.Millisecond)
			}
			if abort && off >= 6400 {
				time.Sleep(30 * time.Millisecond) // let the writer go idle, then cancel the parent context
				cancel()
				return nil
			}
		}
		_ = s.Finalize()
		var errs []error
		deadline := time.After(3 * time.Second)
		for {
			select {
			case ev, ok := <-s.Events():
				if !ok {
					return errs
				}
				if ev.Err != nil {
					errs = append(errs, ev.Err)
				}
				if ev.Type == provider.STTClosed || ev.Type == provider.STTFinal && ev.FromFinalize {
					return errs
				}
			case <-deadline:
				return errs
			}
		}
	}
}

func ttsExercise(newTTS func(url string, log *slog.Logger) (provider.TTSProvider, func(), error), req provider.TTSRequest) func(t *testing.T, url string, log *slog.Logger, abort bool) []error {
	return func(t *testing.T, url string, log *slog.Logger, abort bool) []error {
		p, closeFn, err := newTTS(url, log)
		require.NoError(t, err)
		defer closeFn()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		s, err := p.Synthesize(ctx, req)
		if err != nil {
			return []error{err}
		}
		defer func() { _ = s.Close() }()
		var errs []error
		timeout := time.After(5 * time.Second)
		for {
			select {
			case c, ok := <-s.Chunks():
				if !ok {
					return errs
				}
				if c.Err != nil {
					errs = append(errs, c.Err)
				}
				if abort {
					cancel()
				}
			case <-timeout:
				t.Fatal("TTS stream did not end")
			}
		}
	}
}

func allAdapterCases() []adapterCase {
	openaiCfg := func(url string) openai.Config { return openai.Config{APIKey: secretKey, BaseURL: url} }
	deepseekCfg := func(url string) deepseek.Config { return deepseek.Config{APIKey: secretKey, BaseURL: url} }
	stt := func(vendor, oldKey, code string, open func(url string, log *slog.Logger) (provider.STTProvider, error)) adapterCase {
		return adapterCase{vendor: vendor, capab: "STT", proto: protoWS, oldKey: oldKey, errCode: code, ok: "stt_suite", abortFixture: "stt_suite", exercise: sttExercise(open)}
	}
	tts := func(vendor, oldKey string, p proto, ok string, req provider.TTSRequest, mk func(url string, log *slog.Logger) (provider.TTSProvider, func(), error)) adapterCase {
		return adapterCase{vendor: vendor, capab: "TTS", proto: p, oldKey: oldKey, httpPath: "/v1/text-to-speech/VOICE1/stream", errCode: "bad_" + secretKey, ok: ok, abortFixture: ok,
			exercise: ttsExercise(mk, req)}
	}
	return []adapterCase{
		llmCase("openai", "sk-test", func(url string, log *slog.Logger) (provider.LLMProvider, error) {
			return openai.NewLLM(openaiCfg(url), httpx.NewClient(), log)
		}),
		evalCase("openai", "sk-test", func(url string, log *slog.Logger) (provider.EvaluationProvider, error) {
			return openai.NewEvaluation(openaiCfg(url), httpx.NewClient(), log)
		}),
		embedCase(),
		stt("openai", "sk-test", "bad_"+secretKey, func(url string, log *slog.Logger) (provider.STTProvider, error) {
			return openai.NewSTT(openai.Config{APIKey: secretKey, WSBaseURL: url, STTModel: "m_stt", SyntheticFinalDelay: 30 * time.Millisecond}, httpx.NewClient(), log)
		}),
		llmCase("deepseek", "dk-test", func(url string, log *slog.Logger) (provider.LLMProvider, error) {
			return deepseek.NewLLM(deepseekCfg(url), httpx.NewClient(), log)
		}),
		evalCase("deepseek", "dk-test", func(url string, log *slog.Logger) (provider.EvaluationProvider, error) {
			return deepseek.NewEvaluation(deepseekCfg(url), httpx.NewClient(), log)
		}),
		tts("elevenlabs", "xi-test-key", protoHTTP, "tts_conformance",
			provider.TTSRequest{Text: userSecret, Language: "ja", Model: "m_flash", VoiceID: "VOICE1", SpeakingRate: 1.0},
			func(url string, log *slog.Logger) (provider.TTSProvider, func(), error) {
				p, err := elevenlabs.NewTTS(elevenlabs.Config{APIKey: secretKey, BaseURL: url}, httpx.NewClient(), log)
				return p, func() {}, err
			}),
		stt("elevenlabs", "xi-test-key", "auth_error", func(url string, log *slog.Logger) (provider.STTProvider, error) {
			return elevenlabs.NewSTT(elevenlabs.Config{APIKey: secretKey, WSBaseURL: url, STTModel: "m_stt", SyntheticFinalDelay: 30 * time.Millisecond}, httpx.NewClient(), log)
		}),
		stt("cartesia", "ct-test-key", "bad_"+secretKey, func(url string, log *slog.Logger) (provider.STTProvider, error) {
			return cartesia.NewSTT(cartesia.Config{APIKey: secretKey, WSBaseURL: url, STTModel: "m_ink", Version: "v1"}, httpx.NewClient(), log)
		}),
		tts("cartesia", "ct-test-key", protoWS, "tts_suite",
			provider.TTSRequest{Text: userSecret, Language: "ja", Model: "M1", VoiceID: "V1", SpeakingRate: 1.0, RequestID: "sid:1:0"},
			func(url string, log *slog.Logger) (provider.TTSProvider, func(), error) {
				p, err := cartesia.NewTTS(cartesia.Config{APIKey: secretKey, WSBaseURL: url, Version: "v1"}, httpx.NewClient(), log)
				if err != nil {
					return nil, func() {}, err
				}
				return p, func() { _ = p.Close() }, nil
			}),
	}
}

func (c adapterCase) name() string { return c.vendor + "/" + c.capab }

// script loads a vendor fixture with the vendor's test key replaced by secretKey.
func (c adapterCase) script(t *testing.T, name string) *replay.Script {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(replay.RecordingsDir(t, c.vendor), name+".jsonl"))
	require.NoError(t, err)
	s, err := replay.ParseScript(bytes.ReplaceAll(raw, []byte(c.oldKey), []byte(secretKey)))
	require.NoError(t, err)
	return s
}

// hangScript answers the HTTP path with a 200 whose body only starts after 20 s.
func hangScript(t *testing.T, vendor, path string) *replay.Script {
	t.Helper()
	return inlineScript(t, vendor, []map[string]any{{"http": map[string]any{
		"expect":  map[string]any{"method": "POST", "path": path},
		"respond": map[string]any{"status": 200, "chunks": []map[string]any{{"at_ms": 20000, "text": "{}"}}},
	}}})
}

func inlineScript(t *testing.T, vendor string, lines []map[string]any) *replay.Script {
	t.Helper()
	var b bytes.Buffer
	meta := map[string]any{"meta": map[string]any{"vendor": strings.ToUpper(vendor), "capability": "X", "source": "inline test script", "verified": false}}
	for _, l := range append([]map[string]any{meta}, lines...) {
		raw, err := json.Marshal(l)
		require.NoError(t, err)
		b.Write(raw)
		b.WriteByte('\n')
	}
	s, err := replay.ParseScript(b.Bytes())
	require.NoError(t, err)
	return s
}

// serverFor starts the simulator of the right protocol and returns its URL.
func (c adapterCase) serverFor(t *testing.T, s *replay.Script) string {
	t.Helper()
	if c.proto == protoWS {
		return replay.NewWSServer(t, s).URL
	}
	return replay.NewHTTPServer(t, s).URL
}

// ---- goroutine leaks ---------------------------------------------------------------------

func TestNoGoroutineLeakAfterCancel(t *testing.T) {
	for _, c := range allAdapterCases() {
		t.Run(c.name(), func(t *testing.T) {
			for i := 0; i < 20; i++ {
				var s *replay.Script
				switch {
				case c.abortFixture != "":
					s = c.script(t, c.abortFixture)
				default:
					s = hangScript(t, c.vendor, c.httpPath)
				}
				// The simulator belongs to the parent test and outlives the iteration: a goroutine
				// that only ends because the server went away would be a leak.
				url := c.serverFor(t, s)
				t.Run(fmt.Sprint(i), func(t *testing.T) {
					testutil.NoGoroutineLeak(t, modPath+"internal/provider/"+c.vendor)
					c.exercise(t, url, slog.New(slog.DiscardHandler), true)
				})
			}
		})
	}
}

// ---- secrets and conversation text -------------------------------------------------------

// leakSink collects every place a secret could end up besides returned errors.
type leakSink struct {
	mu   sync.Mutex
	logs bytes.Buffer
}

func (l *leakSink) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.logs.Write(p)
}

func (l *leakSink) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.logs.String()
}

func spanText(rec *testutil.TraceRecorder) string {
	var b strings.Builder
	for _, s := range rec.Spans() {
		fmt.Fprintf(&b, "%s %s %v", s.Name, s.Status.Description, s.Attributes)
		for _, ev := range s.Events {
			fmt.Fprintf(&b, " %s %v", ev.Name, ev.Attributes)
		}
	}
	return b.String()
}

func errText(err error) string {
	var pe *provider.Error
	if errors.As(err, &pe) {
		return fmt.Sprintf("%s|%s|%s|%s|%+v", err.Error(), pe.Message, pe.VendorCode, pe.Kind, pe)
	}
	return err.Error()
}

// failureScript is the vendor failing with a body/message that echoes the key, in the vendor's
// transport: HTTP status codes for HTTP, an in-session error frame for WebSocket.
func (c adapterCase) failureScripts(t *testing.T) map[string]*replay.Script {
	t.Helper()
	out := map[string]*replay.Script{}
	switch {
	case c.proto == protoHTTP:
		body := `{"error":{"message":"Incorrect API key provided: ` + secretKey + `","type":"invalid_request_error","code":"` + secretKey + `"},` +
			`"detail":{"status":"invalid_api_key","message":"` + secretKey + `"},"message":"` + secretKey + `"}`
		for _, status := range []int{401, 429, 500, 503} {
			out[fmt.Sprint(status)] = inlineScript(t, c.vendor, []map[string]any{{"http": map[string]any{
				"expect":  map[string]any{"method": "POST", "path": c.httpPath},
				"respond": map[string]any{"status": status, "headers": map[string]string{"Content-Type": "application/json", "Retry-After": "1"}, "chunks": []map[string]any{{"at_ms": 0, "text": body}}},
			}}})
		}
	case c.capab == "STT":
		raw, err := os.ReadFile(filepath.Join(replay.RecordingsDir(t, c.vendor), "stt_rotation.jsonl"))
		require.NoError(t, err)
		raw = bytes.ReplaceAll(raw, []byte("__CODE__"), []byte(c.errCode))
		for _, msg := range []string{"Session limit reached.", "Session limit reached", "Maximum session duration reached"} {
			raw = bytes.ReplaceAll(raw, []byte(msg), []byte("bad key "+secretKey))
		}
		raw = bytes.ReplaceAll(raw, []byte(c.oldKey), []byte(secretKey))
		s, err := replay.ParseScript(raw)
		require.NoError(t, err)
		out["frame"] = s
	default: // cartesia TTS
		frame := fmt.Sprintf(`{"type":"error","status_code":429,"title":"x","message":"bad key %s","error_code":"%s","request_id":"r","context_id":"sid:1:0"}`, secretKey, c.errCode)
		out["frame"] = inlineScript(t, c.vendor, []map[string]any{
			{"ws": map[string]any{"dir": "on", "match": map[string]any{"context_id": "sid:1:0", "continue": false}, "send": []map[string]any{{"at_ms": 0, "text": frame}}}},
		})
	}
	return out
}

// rejectingWS answers every handshake with status and a body that echoes the key.
func rejectingWS(t *testing.T, status int) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"status_code":` + fmt.Sprint(status) + `,"title":"x","message":"bad key ` + secretKey + `","error_code":"` + secretKey + `","error":"` + secretKey + `"}`))
	}))
	t.Cleanup(srv.Close)
	return "ws" + srv.URL[len("http"):]
}

func TestNoSecretInLogsOrErrors(t *testing.T) {
	for _, c := range allAdapterCases() {
		t.Run(c.name(), func(t *testing.T) {
			type run struct {
				name string
				url  string
				want bool // an error is expected
			}
			var runs []run
			runs = append(runs, run{"success", c.serverFor(t, c.script(t, c.ok)), false})
			for name, s := range c.failureScripts(t) {
				runs = append(runs, run{name, c.serverFor(t, s), true})
			}
			if c.proto == protoWS {
				for _, status := range []int{401, 429, 500} {
					runs = append(runs, run{fmt.Sprintf("handshake_%d", status), rejectingWS(t, status), true})
				}
			}
			for _, r := range runs {
				t.Run(r.name, func(t *testing.T) {
					rec := testutil.NewTraceRecorder(t)
					sink := &leakSink{}
					errs := c.exercise(t, r.url, slog.New(slog.NewTextHandler(sink, &slog.HandlerOptions{Level: slog.LevelDebug})), false)
					if r.want {
						require.NotEmpty(t, errs, "the failing vendor must surface an error")
						require.Contains(t, sink.String(), "vendor_code", "a failed call is logged with the §8.4.0 fields (so this test is not vacuous)")
					} else {
						require.Empty(t, errs, "a normal run ends without error")
					}
					for _, err := range errs {
						require.NotContains(t, errText(err), secretKey, "error text")
						require.NotContains(t, errText(err), userSecret, "error text")
					}
					require.NotContains(t, sink.String(), secretKey, "logs")
					require.NotContains(t, sink.String(), userSecret, "logs must not carry conversation text")
					require.NotContains(t, spanText(rec), secretKey, "span")
					require.NotContains(t, spanText(rec), userSecret, "span must not carry conversation text")
				})
			}
		})
	}
}

// TestConfigNeverPrintsAPIKey: every Config redacts the key under fmt verbs and slog.
func TestConfigNeverPrintsAPIKey(t *testing.T) {
	cfgs := map[string]any{
		"openai":     openai.Config{APIKey: secretKey, BaseURL: "https://x"},
		"deepseek":   deepseek.Config{APIKey: secretKey, BaseURL: "https://x"},
		"elevenlabs": elevenlabs.Config{APIKey: secretKey, BaseURL: "https://x"},
		"cartesia":   cartesia.Config{APIKey: secretKey, WSBaseURL: "wss://x"},
	}
	for name, cfg := range cfgs {
		t.Run(name, func(t *testing.T) {
			for _, verb := range []string{"%v", "%+v", "%#v", "%s"} {
				require.NotContains(t, fmt.Sprintf(verb, cfg), secretKey, verb)
				require.NotContains(t, fmt.Sprintf(verb, &cfg), secretKey, "pointer "+verb)
			}
			var b bytes.Buffer
			slog.New(slog.NewJSONHandler(&b, nil)).Info("cfg", "config", cfg)
			slog.New(slog.NewTextHandler(&b, nil)).Info("cfg", "config", cfg)
			require.NotContains(t, b.String(), secretKey)
		})
	}
}
