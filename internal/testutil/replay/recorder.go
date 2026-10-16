package replay

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

// secretHeaders are never written to a recording (§14.6).
var secretHeaders = []string{"Authorization", "xi-api-key", "X-API-Key", "Ocp-Apim-Subscription-Key", "Cookie", "Set-Cookie", "Proxy-Authorization"}

// volatileHeaders change per request and would make a recording brittle.
var volatileHeaders = []string{"User-Agent", "Accept-Encoding", "Content-Length", "Traceparent", "Tracestate", "Baggage", "Connection"}

// secretQuery names query parameters that carry credentials (`key=`, `api_key=`, ...).
var secretQuery = []string{"key", "api_key", "apikey", "token", "access_token"}

// secretFields names JSON object keys whose values are replaced by the "*" wildcard.
var secretFields = []string{"api_key", "apikey", "authorization", "xi-api-key", "token", "access_token", "secret"}

func hasFold(list []string, k string) bool {
	for _, s := range list {
		if strings.EqualFold(s, k) {
			return true
		}
	}
	return false
}

// Recorder captures real vendor traffic as a replay fixture (MANUAL GATE tooling: it only
// runs when a test is started with `-record` and live keys). Credentials are stripped:
// secret headers and query parameters are dropped and credential-named JSON fields become "*".
type Recorder struct {
	t    testing.TB
	meta Meta
	path string

	mu     sync.Mutex
	lines  []any
	lastWS time.Time
}

// NewRecorder prepares a recording that Flush writes to path.
func NewRecorder(t testing.TB, vendor, capability, path string) *Recorder {
	t.Helper()
	return &Recorder{
		t: t, path: path,
		meta: Meta{Vendor: vendor, Capability: capability, Source: "recorded against the live vendor API", Verified: true},
	}
}

type httpLine struct {
	HTTP *HTTPStep `json:"http"`
}

type wsLine struct {
	WS *WSStep `json:"ws"`
}

// Transport wraps base (nil = http.DefaultTransport); every exchange becomes one `http` line,
// in request order, completed when the response body is closed or fully read.
func (r *Recorder) Transport(base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	return roundTripFunc(func(req *http.Request) (*http.Response, error) {
		var body []byte
		if req.Body != nil {
			body, _ = io.ReadAll(req.Body)
			_ = req.Body.Close()
			req.Body = io.NopCloser(bytes.NewReader(body))
		}
		step := &HTTPStep{Expect: HTTPExpect{
			Method:  req.Method,
			Path:    req.URL.Path,
			Query:   redactQuery(req.URL.Query()),
			Headers: redactHeaders(req.Header, true),
			JSON:    redactJSONBody(body),
		}}
		r.mu.Lock()
		r.lines = append(r.lines, httpLine{HTTP: step})
		r.mu.Unlock()

		resp, err := base.RoundTrip(req)
		if err != nil {
			return nil, err
		}
		step.Respond.Status = resp.StatusCode
		step.Respond.Headers = redactHeaders(http.Header{"Content-Type": resp.Header.Values("Content-Type")}, false)
		resp.Body = &tapBody{rc: resp.Body, step: step, r: r, start: time.Now()}
		return resp, nil
	})
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

type tapBody struct {
	rc    io.ReadCloser
	step  *HTTPStep
	r     *Recorder
	start time.Time
}

func (b *tapBody) Read(p []byte) (int, error) {
	n, err := b.rc.Read(p)
	if n > 0 {
		b.r.mu.Lock()
		b.step.Respond.Chunks = append(b.step.Respond.Chunks, newChunk(int(time.Since(b.start).Milliseconds()), p[:n]))
		b.r.mu.Unlock()
	}
	return n, err
}

func (b *tapBody) Close() error { return b.rc.Close() }

func newChunk(atMs int, data []byte) Chunk {
	c := Chunk{AtMs: atMs}
	if utf8.Valid(data) {
		s := string(data)
		c.Text = &s
	} else {
		s := base64.StdEncoding.EncodeToString(data)
		c.B64 = &s
	}
	return c
}

// Tap records one WebSocket message; dir is from the adapter's side: "send" (adapter sent,
// recorded as a `recv` expectation) or "recv" (adapter received, recorded as a `send`).
// Install it as the adapter's Config.Tap.
func (r *Recorder) Tap(dir string, text bool, data []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	elapsed := 0
	if !r.lastWS.IsZero() {
		elapsed = int(now.Sub(r.lastWS).Milliseconds())
	}
	r.lastWS = now
	data = append([]byte(nil), data...)
	switch dir {
	case "send":
		st := &WSStep{Dir: "recv"}
		if text {
			raw := redactJSONBody(data)
			if raw == nil {
				r.t.Logf("replay: recorder skipped a non-JSON text frame the adapter sent")
				return
			}
			st.JSON = raw
		} else {
			st.Binary, st.Len = true, len(data)
		}
		r.lines = append(r.lines, wsLine{WS: st})
	case "recv":
		c := newChunk(elapsed, data)
		if !text && c.Text != nil { // valid UTF-8 binary stays binary
			s := base64.StdEncoding.EncodeToString(data)
			c.Text, c.B64 = nil, &s
		}
		r.lines = append(r.lines, wsLine{WS: &WSStep{Dir: "send", AtMs: c.AtMs, Text: c.Text, B64: c.B64}})
	default:
		r.t.Errorf("replay: Tap dir must be send or recv, got %q", dir)
	}
}

// Flush writes the JSONL file (meta line first), creating parent directories.
func (r *Recorder) Flush() {
	r.t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(map[string]Meta{"meta": r.meta})
	for _, l := range r.lines {
		if err := enc.Encode(l); err != nil {
			r.t.Fatalf("replay: encode recording: %v", err)
		}
	}
	if err := os.MkdirAll(filepath.Dir(r.path), 0o750); err != nil {
		r.t.Fatalf("replay: %v", err)
	}
	if err := os.WriteFile(r.path, buf.Bytes(), 0o600); err != nil {
		r.t.Fatalf("replay: %v", err)
	}
}

func redactQuery(q url.Values) map[string]string {
	out := map[string]string{}
	for k, v := range q {
		if hasFold(secretQuery, k) || len(v) == 0 {
			continue
		}
		out[k] = v[0]
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func redactHeaders(h http.Header, dropVolatile bool) map[string]string {
	out := map[string]string{}
	for k, v := range h {
		if hasFold(secretHeaders, k) || (dropVolatile && hasFold(volatileHeaders, k)) || len(v) == 0 {
			continue
		}
		out[http.CanonicalHeaderKey(k)] = v[0]
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// redactJSONBody returns body as JSON with credential-named fields wildcarded, or nil when
// body is empty or not JSON.
func redactJSONBody(body []byte) json.RawMessage {
	if len(bytes.TrimSpace(body)) == 0 {
		return nil
	}
	var v any
	if err := json.Unmarshal(body, &v); err != nil {
		return nil
	}
	out, err := json.Marshal(redactValue(v))
	if err != nil {
		return nil
	}
	return out
}

func redactValue(v any) any {
	switch x := v.(type) {
	case map[string]any:
		for k, val := range x {
			if hasFold(secretFields, k) {
				x[k] = "*"
				continue
			}
			x[k] = redactValue(val)
		}
	case []any:
		for i := range x {
			x[i] = redactValue(x[i])
		}
	}
	return v
}
