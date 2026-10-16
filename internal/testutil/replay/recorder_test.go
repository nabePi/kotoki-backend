package replay_test

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/testutil/replay"
)

type stubTransport struct{ got *http.Request }

func (s *stubTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	s.got = r
	return &http.Response{
		StatusCode: 200,
		Header:     http.Header{"Content-Type": {"text/event-stream"}, "Set-Cookie": {"sid=SECRET"}, "X-Request-Id": {"r1"}},
		Body:       io.NopCloser(strings.NewReader("data: {\"a\":1}\n\ndata: [DONE]\n\n")),
		Request:    r,
	}, nil
}

func TestRecorderRedactsSecrets(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rec", "llm_basic.jsonl")
	rec := replay.NewRecorder(t, "ELEVENLABS", "TTS", path)
	stub := &stubTransport{}
	hc := &http.Client{Transport: rec.Transport(stub)}

	req, _ := http.NewRequest(http.MethodPost, "http://vendor.test/v1/tts?key=SECRET&api_key=SECRET&output_format=pcm_24000",
		strings.NewReader(`{"text":"hi","api_key":"SECRET","nested":{"Authorization":"Bearer SECRET"}}`))
	req.Header.Set("xi-api-key", "SECRET")
	req.Header.Set("Authorization", "Bearer SECRET")
	req.Header.Set("X-API-Key", "SECRET")
	req.Header.Set("Ocp-Apim-Subscription-Key", "SECRET")
	req.Header.Set("Content-Type", "application/json")
	resp, err := hc.Do(req)
	require.NoError(t, err)
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	require.Equal(t, "data: {\"a\":1}\n\ndata: [DONE]\n\n", string(body), "caller still sees the real body")
	// the vendor still received the real credentials
	require.Equal(t, "SECRET", stub.got.Header.Get("xi-api-key"))
	rec.Flush()

	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	require.NotContains(t, string(raw), "SECRET")
	require.Contains(t, string(raw), "output_format")
	require.Contains(t, string(raw), `"verified":true`)

	// the recording is a valid fixture and replays: the redacted fields became wildcards
	s := replay.LoadScriptString(t, string(raw))
	require.Equal(t, "ELEVENLABS", s.Meta.Vendor)
	srv := replay.NewHTTPServer(t, s)
	r2, err := http.Post(srv.URL+"/v1/tts?output_format=pcm_24000", "application/json",
		strings.NewReader(`{"text":"hi","api_key":"other","nested":{"Authorization":"x"}}`))
	require.NoError(t, err)
	defer func() { _ = r2.Body.Close() }()
	b2, _ := io.ReadAll(r2.Body)
	require.Equal(t, "data: {\"a\":1}\n\ndata: [DONE]\n\n", string(b2))
	srv.AssertDone()
}

func TestRecorderTapRoundTripsWebSocket(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stt_basic.jsonl")
	rec := replay.NewRecorder(t, "CARTESIA", "STT", path)
	rec.Tap("send", true, []byte(`{"type":"start","token":"SECRET"}`))
	rec.Tap("recv", true, []byte(`{"type":"ready"}`))
	rec.Tap("send", false, []byte{1, 2, 3, 4})
	rec.Tap("recv", false, []byte{9, 8})
	rec.Flush()
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	require.NotContains(t, string(raw), "SECRET")

	srv := replay.NewWSServer(t, replay.LoadScriptString(t, string(raw)))
	c := dial(t, srv.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, c.Write(ctx, websocket.MessageText, []byte(`{"type":"start","token":"anything"}`)))
	require.JSONEq(t, `{"type":"ready"}`, readText(t, c))
	require.NoError(t, c.Write(ctx, websocket.MessageBinary, []byte{1, 2, 3, 4}))
	typ, b, err := c.Read(ctx)
	require.NoError(t, err)
	require.Equal(t, websocket.MessageBinary, typ)
	require.Equal(t, []byte{9, 8}, b)
	srv.AssertDone()
}
