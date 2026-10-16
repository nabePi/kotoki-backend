package cartesia_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/provider"
	"github.com/nabePi/kotoki-backend/internal/provider/cartesia"
	"github.com/nabePi/kotoki-backend/internal/provider/httpx"
	"github.com/nabePi/kotoki-backend/internal/testutil/replay"
)

func newTTS(t *testing.T, wsURL string, mod func(*cartesia.Config)) *cartesia.TTS {
	t.Helper()
	cfg := cartesia.Config{APIKey: testKey, WSBaseURL: wsURL, Version: "v1"}
	if mod != nil {
		mod(&cfg)
	}
	tts, err := cartesia.NewTTS(cfg, httpx.NewClient(), bufLogger(&logBuf{}))
	require.NoError(t, err)
	t.Cleanup(func() { _ = tts.Close() })
	return tts
}

func ttsReq() provider.TTSRequest {
	return provider.TTSRequest{Text: "こんにちは。", Language: "ja", Model: "M1", VoiceID: "V1", SpeakingRate: 1.0, RequestID: "sid:1:0"}
}

// ttsScript builds an inline fixture: a rule answering every generate message with the given
// server messages (each a JSON text).
func ttsScript(t *testing.T, lines ...string) *replay.Script {
	t.Helper()
	head := `{"meta":{"vendor":"CARTESIA","capability":"TTS","source":"inline test fixture","verified":false}}`
	return replay.LoadScriptString(t, strings.Join(append([]string{head}, lines...), "\n"))
}

const matchGenerate = `{"model_id":"*","transcript":"*"}`

func chunkMsg(id string, n int) string {
	return `{"type":"chunk","data":"` + base64.StdEncoding.EncodeToString(make([]byte, n)) + `","done":false,"status_code":206,"context_id":"` + id + `"}`
}

func doneMsg(id string) string {
	return `{"type":"done","done":true,"status_code":200,"context_id":"` + id + `"}`
}

func drainTTS(t *testing.T, ch <-chan provider.TTSChunk) []provider.TTSChunk {
	t.Helper()
	var out []provider.TTSChunk
	deadline := time.After(5 * time.Second)
	for {
		select {
		case c, ok := <-ch:
			if !ok {
				return out
			}
			out = append(out, c)
		case <-deadline:
			t.Fatal("stream did not finish")
		}
	}
}

func totalBytes(cs []provider.TTSChunk) int {
	n := 0
	for _, c := range cs {
		n += len(c.PCM)
	}
	return n
}

// srvMsg is a client message seen by fakeSrv.
type srvMsg struct {
	Conn int
	At   time.Time
	Body map[string]any
}

// srvConn is one accepted connection of fakeSrv.
type srvConn struct {
	Idx int
	ws  *websocket.Conn
	ctx context.Context
}

func (c *srvConn) send(t *testing.T, text string) {
	t.Helper()
	_ = c.ws.Write(c.ctx, websocket.MessageText, []byte(text))
}

// fakeSrv is a programmable Cartesia TTS server for the tests that need several connections
// or reactions the replay simulator cannot script (it serves one scripted connection).
type fakeSrv struct {
	t   *testing.T
	URL string

	mu         sync.Mutex
	handshakes []http.Header
	queries    []url.Values
	conns      int
	msgs       []srvMsg
	onMsg      func(c *srvConn, m map[string]any)
	hs         *httptest.Server
}

func newFakeSrv(t *testing.T, onMsg func(c *srvConn, m map[string]any)) *fakeSrv {
	t.Helper()
	f := &fakeSrv{t: t, onMsg: onMsg}
	f.hs = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.hs.Close)
	f.URL = "ws" + f.hs.URL[len("http"):]
	return f
}

func (f *fakeSrv) serve(w http.ResponseWriter, r *http.Request) {
	ws, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer func() { _ = ws.CloseNow() }()
	f.mu.Lock()
	idx := f.conns
	f.conns++
	f.handshakes = append(f.handshakes, r.Header.Clone())
	f.queries = append(f.queries, r.URL.Query())
	f.mu.Unlock()
	sc := &srvConn{Idx: idx, ws: ws, ctx: r.Context()}
	for {
		_, data, err := ws.Read(r.Context())
		if err != nil {
			return
		}
		var m map[string]any
		if json.Unmarshal(data, &m) != nil {
			continue
		}
		f.mu.Lock()
		f.msgs = append(f.msgs, srvMsg{Conn: idx, At: time.Now(), Body: m})
		f.mu.Unlock()
		if f.onMsg != nil {
			f.onMsg(sc, m)
		}
	}
}

func (f *fakeSrv) connCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.conns
}

func (f *fakeSrv) messages() []srvMsg {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]srvMsg(nil), f.msgs...)
}

func (f *fakeSrv) waitMsg(match func(srvMsg) bool) srvMsg {
	f.t.Helper()
	var got srvMsg
	require.Eventually(f.t, func() bool {
		for _, m := range f.messages() {
			if match(m) {
				got = m
				return true
			}
		}
		return false
	}, 3*time.Second, 2*time.Millisecond)
	return got
}

func isCancel(m srvMsg) bool { return m.Body["cancel"] == true }

// replyAudio answers a generate message with n chunks of size bytes then done.
func replyAudio(t *testing.T, n, size int) func(c *srvConn, m map[string]any) {
	return func(c *srvConn, m map[string]any) {
		id, _ := m["context_id"].(string)
		if _, ok := m["transcript"]; !ok {
			return
		}
		for i := 0; i < n; i++ {
			c.send(t, chunkMsg(id, size))
		}
		c.send(t, doneMsg(id))
	}
}
