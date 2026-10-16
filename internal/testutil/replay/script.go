package replay

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nabePi/kotoki-backend/internal/testutil"
)

// Meta is line 1 of every fixture. Verified is false for fixtures copied from vendor
// documentation and true only for fixtures written by the Recorder against the real vendor.
type Meta struct {
	Vendor     string `json:"vendor"`
	Capability string `json:"capability"`
	Source     string `json:"source"`
	Verified   bool   `json:"verified"`
}

// Chunk is one piece of an HTTP response body or one server-to-client WebSocket message.
// AtMs is relative to the start of the response (HTTP) or to the moment the step becomes
// active (WebSocket). Exactly one of Text and B64 is set.
type Chunk struct {
	AtMs int     `json:"at_ms"`
	Text *string `json:"text,omitempty"`
	B64  *string `json:"b64,omitempty"`
}

func (c Chunk) bytes() (data []byte, text bool, err error) {
	switch {
	case c.Text != nil && c.B64 != nil:
		return nil, false, fmt.Errorf("both text and b64 set")
	case c.Text != nil:
		return []byte(*c.Text), true, nil
	case c.B64 != nil:
		b, err := base64.StdEncoding.DecodeString(*c.B64)
		return b, false, err
	}
	return nil, false, fmt.Errorf("neither text nor b64 set")
}

// HTTPExpect is what the simulator requires of the next request. Query and Headers values
// of "*" mean "present with any value"; JSON is a subset the body must contain.
type HTTPExpect struct {
	Method  string            `json:"method"`
	Path    string            `json:"path"`
	Query   map[string]string `json:"query,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	JSON    json.RawMessage   `json:"json,omitempty"`
}

// HTTPRespond is the scripted response.
type HTTPRespond struct {
	Status  int               `json:"status"`
	Headers map[string]string `json:"headers,omitempty"`
	Chunks  []Chunk           `json:"chunks,omitempty"`
}

// HTTPStep is one `http` line: requests are served in order; Repeat serves without limit.
type HTTPStep struct {
	Expect  HTTPExpect  `json:"expect"`
	Respond HTTPRespond `json:"respond"`
	Repeat  bool        `json:"repeat,omitempty"`
	line    int
}

// WSUpgrade is the `ws_upgrade` line: what the handshake request must carry.
type WSUpgrade struct {
	Path    string            `json:"path,omitempty"`
	Query   map[string]string `json:"query,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	line    int
}

// WSStep is one `ws` line. Dir selects the meaning:
//
//	send  server -> client: Text|B64 after AtMs
//	recv  server expects a client message: JSON subset, or Binary (+ Len)
//	on    reactive rule: a client message matching Match is answered with Send, Times times
//	      (0 = unlimited); a message handled by a rule is not delivered to recv steps
//	close server closes the connection with Code
type WSStep struct {
	Dir    string          `json:"dir"`
	AtMs   int             `json:"at_ms,omitempty"`
	Text   *string         `json:"text,omitempty"`
	B64    *string         `json:"b64,omitempty"`
	JSON   json.RawMessage `json:"json,omitempty"`
	Binary bool            `json:"binary,omitempty"`
	Len    int             `json:"len,omitempty"`
	Match  json.RawMessage `json:"match,omitempty"`
	Send   []Chunk         `json:"send,omitempty"`
	Times  int             `json:"times,omitempty"`
	Code   int             `json:"code,omitempty"`
	line   int
}

// Script is a parsed fixture.
type Script struct {
	Meta      Meta
	HTTP      []*HTTPStep
	WSUpgrade *WSUpgrade
	WS        []*WSStep
}

// RecordingsDir is internal/provider/<vendor>/testdata/recordings under the module root.
func RecordingsDir(t testing.TB, vendor string) string {
	t.Helper()
	return filepath.Join(testutil.ModuleRoot(t), "internal", "provider", vendor, "testdata", "recordings")
}

// LoadScript reads <dir>/<name>.jsonl.
func LoadScript(t testing.TB, dir, name string) *Script {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, name+".jsonl")) //nolint:gosec // test fixture path
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	s, err := ParseScript(raw)
	if err != nil {
		t.Fatalf("replay: %s: %v", name, err)
	}
	return s
}

// LoadScriptString parses an inline fixture; for tests only.
func LoadScriptString(t testing.TB, jsonl string) *Script {
	t.Helper()
	s, err := ParseScript([]byte(jsonl))
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	return s
}

// ParseScript parses JSONL; blank lines are skipped, line numbers in errors are 1-based.
func ParseScript(raw []byte) (*Script, error) {
	s := &Script{}
	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(make([]byte, 0, 64<<10), 16<<20)
	n, seenMeta := 0, false
	for sc.Scan() {
		n++
		text := strings.TrimSpace(sc.Text())
		if text == "" {
			continue
		}
		var line map[string]json.RawMessage
		if err := json.Unmarshal([]byte(text), &line); err != nil {
			return nil, fmt.Errorf("line %d: %w", n, err)
		}
		if len(line) != 1 {
			return nil, fmt.Errorf("line %d: want exactly one of meta|http|ws_upgrade|ws, got %d keys", n, len(line))
		}
		for key, val := range line {
			if !seenMeta && key != "meta" {
				return nil, fmt.Errorf("line %d: first line must be meta", n)
			}
			if err := s.add(n, key, val); err != nil {
				return nil, fmt.Errorf("line %d: %w", n, err)
			}
			seenMeta = true
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if !seenMeta {
		return nil, fmt.Errorf("empty script: meta line required")
	}
	return s, nil
}

func strictUnmarshal(val json.RawMessage, v any) error {
	dec := json.NewDecoder(bytes.NewReader(val))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

func (s *Script) add(n int, key string, val json.RawMessage) error {
	switch key {
	case "meta":
		if s.Meta != (Meta{}) {
			return fmt.Errorf("duplicate meta")
		}
		if err := strictUnmarshal(val, &s.Meta); err != nil {
			return err
		}
		if s.Meta.Vendor == "" || s.Meta.Capability == "" || s.Meta.Source == "" {
			return fmt.Errorf("meta needs vendor, capability and source")
		}
	case "http":
		var st HTTPStep
		if err := strictUnmarshal(val, &st); err != nil {
			return err
		}
		if st.Expect.Method == "" || st.Expect.Path == "" {
			return fmt.Errorf("http.expect needs method and path")
		}
		for i, c := range st.Respond.Chunks {
			if _, _, err := c.bytes(); err != nil {
				return fmt.Errorf("http.respond.chunks[%d]: %w", i, err)
			}
		}
		st.line = n
		s.HTTP = append(s.HTTP, &st)
	case "ws_upgrade":
		if s.WSUpgrade != nil {
			return fmt.Errorf("duplicate ws_upgrade")
		}
		var u WSUpgrade
		if err := strictUnmarshal(val, &u); err != nil {
			return err
		}
		u.line = n
		s.WSUpgrade = &u
	case "ws":
		var st WSStep
		if err := strictUnmarshal(val, &st); err != nil {
			return err
		}
		if err := st.validate(); err != nil {
			return err
		}
		st.line = n
		s.WS = append(s.WS, &st)
	default:
		return fmt.Errorf("unknown key %q", key)
	}
	return nil
}

func (st *WSStep) validate() error {
	switch st.Dir {
	case "send":
		if _, _, err := (Chunk{Text: st.Text, B64: st.B64}).bytes(); err != nil {
			return fmt.Errorf("ws send: %w", err)
		}
	case "recv":
		if st.Binary == (len(st.JSON) > 0) {
			return fmt.Errorf("ws recv: exactly one of json and binary")
		}
	case "on":
		if len(st.Match) == 0 {
			return fmt.Errorf("ws on: match required")
		}
		for i, c := range st.Send {
			if _, _, err := c.bytes(); err != nil {
				return fmt.Errorf("ws on: send[%d]: %w", i, err)
			}
		}
	case "close":
		if st.Code == 0 {
			return fmt.Errorf("ws close: code required")
		}
	default:
		return fmt.Errorf("ws: unknown dir %q", st.Dir)
	}
	return nil
}
