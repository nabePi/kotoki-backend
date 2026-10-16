package replay

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"sync"
	"testing"
	"time"
)

// Request is one request the simulator received.
type Request struct {
	Method string
	Path   string
	Query  url.Values
	Header http.Header
	Body   []byte
}

// Message is one WebSocket message the simulator received from the client.
type Message struct {
	Text bool
	Data []byte
	At   time.Time
}

// Server is a vendor simulator driven by a Script (HTTP or WebSocket).
type Server struct {
	// URL is http://host:port (HTTP) or ws://host:port (WebSocket).
	URL string

	t   testing.TB
	s   *Script
	srv *httptest.Server

	mu       sync.Mutex
	requests []Request
	messages []Message
	upgrade  Request
	httpIdx  int
	httpUsed map[*HTTPStep]bool

	stop chan struct{}
	wg   sync.WaitGroup
	ws   *wsState
}

// Requests returns the HTTP requests received so far (for WebSocket: none).
func (sv *Server) Requests() []Request {
	sv.mu.Lock()
	defer sv.mu.Unlock()
	return append([]Request(nil), sv.requests...)
}

// ClientMessages returns the WebSocket messages the client sent, in arrival order.
func (sv *Server) ClientMessages() []Message {
	sv.mu.Lock()
	defer sv.mu.Unlock()
	return append([]Message(nil), sv.messages...)
}

// UpgradeRequest returns the WebSocket handshake request (path, query, headers).
func (sv *Server) UpgradeRequest() Request {
	sv.mu.Lock()
	defer sv.mu.Unlock()
	return sv.upgrade
}

func newServer(t testing.TB, s *Script) *Server {
	t.Helper()
	sv := &Server{t: t, s: s, httpUsed: map[*HTTPStep]bool{}, stop: make(chan struct{})}
	t.Cleanup(func() {
		close(sv.stop)
		sv.srv.Close()
		sv.wg.Wait()
	})
	return sv
}

// NewHTTPServer serves the script's `http` lines in order.
func NewHTTPServer(t testing.TB, s *Script) *Server {
	t.Helper()
	sv := newServer(t, s)
	sv.srv = httptest.NewServer(http.HandlerFunc(sv.serveHTTP))
	sv.URL = sv.srv.URL
	return sv
}

func (sv *Server) serveHTTP(w http.ResponseWriter, r *http.Request) {
	sv.wg.Add(1)
	defer sv.wg.Done()
	body, _ := io.ReadAll(r.Body)
	req := Request{Method: r.Method, Path: r.URL.Path, Query: r.URL.Query(), Header: r.Header.Clone(), Body: body}

	sv.mu.Lock()
	sv.requests = append(sv.requests, req)
	var step *HTTPStep
	if sv.httpIdx < len(sv.s.HTTP) {
		step = sv.s.HTTP[sv.httpIdx]
		sv.httpUsed[step] = true
		if !step.Repeat {
			sv.httpIdx++
		}
	}
	sv.mu.Unlock()

	if step == nil {
		sv.t.Errorf("replay: unexpected request %s %s: script has no more http lines", r.Method, r.URL.Path)
		http.Error(w, "replay: no more scripted responses", http.StatusInternalServerError)
		return
	}
	if problems := matchHTTP(step.Expect, req); len(problems) > 0 {
		for _, p := range problems {
			sv.t.Errorf("replay: line %d (%s %s): %s", step.line, step.Expect.Method, step.Expect.Path, p)
		}
		http.Error(w, "replay: request does not match script", http.StatusInternalServerError)
		return
	}
	sv.respond(w, r, step.Respond)
}

func (sv *Server) respond(w http.ResponseWriter, r *http.Request, resp HTTPRespond) {
	for k, v := range resp.Headers {
		w.Header().Set(k, v)
	}
	status := resp.Status
	if status == 0 {
		status = http.StatusOK
	}
	w.WriteHeader(status)
	fl, _ := w.(http.Flusher)
	if fl != nil {
		fl.Flush()
	}
	start := time.Now()
	for _, c := range resp.Chunks {
		if !sleepUntil(start.Add(time.Duration(c.AtMs)*time.Millisecond), r.Context().Done(), sv.stop) {
			return
		}
		data, _, _ := c.bytes()
		if _, err := w.Write(data); err != nil {
			return
		}
		if fl != nil {
			fl.Flush()
		}
	}
}

// sleepUntil waits until t, reporting false when either stop channel fires first.
func sleepUntil(t time.Time, stopA, stopB <-chan struct{}) bool {
	if d := time.Until(t); d > 0 {
		timer := time.NewTimer(d)
		defer timer.Stop()
		select {
		case <-timer.C:
			return true
		case <-stopA:
			return false
		case <-stopB:
			return false
		}
	}
	select {
	case <-stopA:
		return false
	case <-stopB:
		return false
	default:
		return true
	}
}

func matchHTTP(e HTTPExpect, r Request) []string {
	var out []string
	if e.Method != r.Method {
		out = append(out, fmt.Sprintf("method: want %s got %s", e.Method, r.Method))
	}
	if e.Path != r.Path {
		out = append(out, fmt.Sprintf("path: want %s got %s", e.Path, r.Path))
	}
	out = append(out, matchKV("query", e.Query, func(k string) (string, bool) {
		v, ok := r.Query[k]
		if !ok || len(v) == 0 {
			return "", false
		}
		return v[0], true
	})...)
	out = append(out, matchKV("headers", e.Headers, func(k string) (string, bool) {
		v := r.Header.Values(k)
		if len(v) == 0 {
			return "", false
		}
		return v[0], true
	})...)
	if len(e.JSON) > 0 {
		out = append(out, matchJSONBody(e.JSON, r.Body)...)
	}
	return out
}

func matchKV(what string, want map[string]string, get func(string) (string, bool)) []string {
	keys := make([]string, 0, len(want))
	for k := range want {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var out []string
	for _, k := range keys {
		got, ok := get(k)
		switch {
		case !ok:
			out = append(out, fmt.Sprintf("%s.%s: missing", what, k))
		case want[k] != "*" && want[k] != got:
			out = append(out, fmt.Sprintf("%s.%s: want %q got %q", what, k, want[k], got))
		}
	}
	return out
}

func matchJSONBody(want json.RawMessage, body []byte) []string {
	var w, g any
	if err := json.Unmarshal(want, &w); err != nil {
		return []string{fmt.Sprintf("json: bad expectation: %v", err)}
	}
	if err := json.Unmarshal(body, &g); err != nil {
		return []string{fmt.Sprintf("json: body is not JSON: %v", err)}
	}
	return jsonSubset("json", w, g)
}

// jsonSubset reports where got does not contain want: objects match by subset, arrays element
// by element with equal length, scalars by equality, and the string "*" matches any present value.
func jsonSubset(path string, want, got any) []string {
	if s, ok := want.(string); ok && s == "*" {
		return nil
	}
	switch w := want.(type) {
	case map[string]any:
		g, ok := got.(map[string]any)
		if !ok {
			return []string{fmt.Sprintf("%s: want object got %s", path, jsonKind(got))}
		}
		keys := make([]string, 0, len(w))
		for k := range w {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var out []string
		for _, k := range keys {
			gv, ok := g[k]
			if !ok {
				out = append(out, fmt.Sprintf("%s.%s: missing", path, k))
				continue
			}
			out = append(out, jsonSubset(path+"."+k, w[k], gv)...)
		}
		return out
	case []any:
		g, ok := got.([]any)
		if !ok {
			return []string{fmt.Sprintf("%s: want array got %s", path, jsonKind(got))}
		}
		if len(w) != len(g) {
			return []string{fmt.Sprintf("%s: want %d elements got %d", path, len(w), len(g))}
		}
		var out []string
		for i := range w {
			out = append(out, jsonSubset(fmt.Sprintf("%s[%d]", path, i), w[i], g[i])...)
		}
		return out
	}
	wb, _ := json.Marshal(want)
	gb, _ := json.Marshal(got)
	if !bytes.Equal(wb, gb) {
		return []string{fmt.Sprintf("%s: want %s got %s", path, wb, gb)}
	}
	return nil
}

func jsonKind(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case map[string]any:
		return "object"
	case []any:
		return "array"
	case string:
		return "string"
	case bool:
		return "bool"
	}
	return "number"
}

// AssertDone fails the test when a non-repeat script line was not used. Mismatches found
// while serving were already reported with t.Errorf.
func (sv *Server) AssertDone() {
	sv.t.Helper()
	sv.mu.Lock()
	for _, st := range sv.s.HTTP {
		if !st.Repeat && !sv.httpUsed[st] {
			sv.t.Errorf("replay: line %d (%s %s) was never requested", st.line, st.Expect.Method, st.Expect.Path)
		}
	}
	sv.mu.Unlock()
	if sv.ws != nil {
		sv.ws.assertDone(sv)
	}
}
