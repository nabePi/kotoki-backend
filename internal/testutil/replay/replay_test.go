package replay_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/testutil/replay"
)

// recTB records Errorf instead of failing, for tests that assert the simulator reports a mismatch.
type recTB struct {
	testing.TB
	mu   sync.Mutex
	errs []string
}

func (r *recTB) Errorf(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.errs = append(r.errs, strings.TrimSpace(fmt.Sprintf(format, args...)))
}

func (r *recTB) all() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.Join(r.errs, "\n")
}

const metaLine = `{"meta":{"vendor":"OPENAI","capability":"LLM","source":"x","verified":false}}` + "\n"

func post(t *testing.T, url, body string) (*http.Response, string) {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp, string(b)
}

func TestHTTPSimMatchesSubsetAndStreams(t *testing.T) {
	s := replay.LoadScriptString(t, `{"meta":{"vendor":"OPENAI","capability":"LLM","source":"x","verified":false}}
{"http":{"expect":{"method":"POST","path":"/v1/chat/completions","json":{"stream":true,"model":"*"}},"respond":{"status":200,"chunks":[{"at_ms":0,"text":"data: {\"a\":1}\n\n"},{"at_ms":5,"text":"data: [DONE]\n\n"}]}}}`)
	srv := replay.NewHTTPServer(t, s)
	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"stream":true,"model":"m","extra":1}`))
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	require.Equal(t, "data: {\"a\":1}\n\ndata: [DONE]\n\n", string(b))
	srv.AssertDone()

	reqs := srv.Requests()
	require.Len(t, reqs, 1)
	require.Equal(t, "/v1/chat/completions", reqs[0].Path)
	require.JSONEq(t, `{"stream":true,"model":"m","extra":1}`, string(reqs[0].Body))
}

func TestHTTPSimChunkTiming(t *testing.T) {
	s := replay.LoadScriptString(t, metaLine+
		`{"http":{"expect":{"method":"GET","path":"/s"},"respond":{"status":200,"chunks":[{"at_ms":0,"text":"a"},{"at_ms":120,"text":"b"}]}}}`)
	srv := replay.NewHTTPServer(t, s)
	start := time.Now()
	resp, err := http.Get(srv.URL + "/s")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	first := make([]byte, 1)
	_, err = io.ReadFull(resp.Body, first)
	require.NoError(t, err)
	require.Less(t, time.Since(start), 100*time.Millisecond, "first chunk is not delayed")
	rest, _ := io.ReadAll(resp.Body)
	require.Equal(t, "b", string(rest))
	require.GreaterOrEqual(t, time.Since(start), 120*time.Millisecond, "second chunk waits for at_ms")
}

func TestHTTPSimBinaryChunkStatusAndHeaders(t *testing.T) {
	s := replay.LoadScriptString(t, metaLine+
		`{"http":{"expect":{"method":"POST","path":"/tts","query":{"output_format":"pcm_24000","key":"*"},"headers":{"X-Test":"yes"}},"respond":{"status":429,"headers":{"Retry-After":"2"},"chunks":[{"at_ms":0,"b64":"AAEC"}]}}}`)
	srv := replay.NewHTTPServer(t, s)
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/tts?output_format=pcm_24000&key=abc", strings.NewReader("{}"))
	req.Header.Set("X-Test", "yes")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	require.Equal(t, 429, resp.StatusCode)
	require.Equal(t, "2", resp.Header.Get("Retry-After"))
	require.Equal(t, []byte{0, 1, 2}, b)
	srv.AssertDone()
}

func TestHTTPSimRepeatServesWithoutLimit(t *testing.T) {
	s := replay.LoadScriptString(t, metaLine+
		`{"http":{"expect":{"method":"GET","path":"/r"},"respond":{"status":200,"chunks":[{"at_ms":0,"text":"ok"}]},"repeat":true}}`)
	srv := replay.NewHTTPServer(t, s)
	for range 3 {
		resp, err := http.Get(srv.URL + "/r")
		require.NoError(t, err)
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		require.Equal(t, "ok", string(b))
	}
	require.Len(t, srv.Requests(), 3)
	srv.AssertDone()
}

func TestHTTPSimMismatchFailsTest(t *testing.T) {
	cases := []struct{ name, script, path, body, wantErr string }{
		{"json value", `{"http":{"expect":{"method":"POST","path":"/p","json":{"a":{"b":1}}},"respond":{"status":200}}}`, "/p", `{"a":{"b":2}}`, "json.a.b: want 1 got 2"},
		{"json missing", `{"http":{"expect":{"method":"POST","path":"/p","json":{"a":"*"}},"respond":{"status":200}}}`, "/p", `{}`, "json.a: missing"},
		{"path", `{"http":{"expect":{"method":"POST","path":"/p"},"respond":{"status":200}}}`, "/other", `{}`, "path: want /p got /other"},
		{"array length", `{"http":{"expect":{"method":"POST","path":"/p","json":{"m":[1,2]}},"respond":{"status":200}}}`, "/p", `{"m":[1]}`, "json.m: want 2 elements got 1"},
		{"query", `{"http":{"expect":{"method":"POST","path":"/p","query":{"q":"1"}},"respond":{"status":200}}}`, "/p?q=2", `{}`, `query.q: want "1" got "2"`},
		{"extra request", ``, "/p", `{}`, "unexpected request POST /p"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rt := &recTB{TB: t}
			srv := replay.NewHTTPServer(rt, replay.LoadScriptString(t, metaLine+tc.script))
			resp, _ := post(t, srv.URL+tc.path, tc.body)
			require.Equal(t, http.StatusInternalServerError, resp.StatusCode)
			require.Contains(t, rt.all(), tc.wantErr)
			if tc.script != "" {
				require.Contains(t, rt.all(), "line 2", "mismatch names the script line")
			}
		})
	}
}

func TestHTTPSimAssertDoneFailsWhenUnused(t *testing.T) {
	rt := &recTB{TB: t}
	srv := replay.NewHTTPServer(rt, replay.LoadScriptString(t, metaLine+
		`{"http":{"expect":{"method":"GET","path":"/never"},"respond":{"status":200}}}`))
	srv.AssertDone()
	require.Contains(t, rt.all(), "was never requested")
}

func TestParseScriptRejectsBadFixtures(t *testing.T) {
	for name, tc := range map[string]struct{ in, want string }{
		"empty":          {``, "meta line required"},
		"meta not first": {`{"http":{"expect":{"method":"GET","path":"/"},"respond":{"status":200}}}`, "first line must be meta"},
		"meta fields":    {`{"meta":{"vendor":"X"}}`, "meta needs vendor"},
		"unknown key":    {metaLine + `{"bogus":{}}`, `unknown key "bogus"`},
		"two keys":       {metaLine + `{"http":{},"ws":{}}`, "exactly one of"},
		"unknown field":  {metaLine + `{"http":{"expect":{"method":"GET","path":"/","oops":1},"respond":{}}}`, "oops"},
		"bad chunk":      {metaLine + `{"http":{"expect":{"method":"GET","path":"/"},"respond":{"chunks":[{"at_ms":0}]}}}`, "neither text nor b64"},
		"ws dir":         {metaLine + `{"ws":{"dir":"sideways"}}`, "unknown dir"},
		"recv both":      {metaLine + `{"ws":{"dir":"recv","json":{},"binary":true}}`, "exactly one of json and binary"},
		"close code":     {metaLine + `{"ws":{"dir":"close"}}`, "code required"},
		"not json":       {metaLine + `nope`, "line 2"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := replay.ParseScript([]byte(tc.in))
			require.ErrorContains(t, err, tc.want)
		})
	}
}

func TestLoadScriptAndRecordingsDir(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "llm_basic.jsonl"), []byte(metaLine), 0o600))
	s := replay.LoadScript(t, dir, "llm_basic")
	require.Equal(t, "OPENAI", s.Meta.Vendor)
	require.False(t, s.Meta.Verified)
	d := replay.RecordingsDir(t, "openai")
	require.True(t, strings.HasSuffix(filepath.ToSlash(d), "internal/provider/openai/testdata/recordings"), d)
}

func dial(t *testing.T, url string) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	c, _, err := websocket.Dial(ctx, url, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.CloseNow() })
	return c
}

func readText(t *testing.T, c *websocket.Conn) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	typ, b, err := c.Read(ctx)
	require.NoError(t, err)
	require.Equal(t, websocket.MessageText, typ)
	return string(b)
}

func TestWSSimReactiveRule(t *testing.T) {
	s := replay.LoadScriptString(t, `{"meta":{"vendor":"OPENAI","capability":"STT","source":"x","verified":false}}
{"ws":{"dir":"send","at_ms":0,"text":"{\"type\":\"ready\"}"}}
{"ws":{"dir":"on","match":{"commit":true},"send":[{"at_ms":0,"text":"{\"type\":\"final\"}"}],"times":0}}
{"ws":{"dir":"recv","json":{"type":"start","model":"*"}}}
{"ws":{"dir":"recv","binary":true,"len":4}}`)
	srv := replay.NewWSServer(t, s)
	c := dial(t, srv.URL)
	ctx := context.Background()

	require.JSONEq(t, `{"type":"ready"}`, readText(t, c))
	require.NoError(t, c.Write(ctx, websocket.MessageText, []byte(`{"type":"start","model":"m"}`)))
	require.NoError(t, c.Write(ctx, websocket.MessageBinary, []byte{1, 2, 3, 4}))
	for range 2 { // the rule answers every matching message
		require.NoError(t, c.Write(ctx, websocket.MessageText, []byte(`{"commit":true,"x":1}`)))
		require.JSONEq(t, `{"type":"final"}`, readText(t, c))
	}
	require.Eventually(t, func() bool { return len(srv.ClientMessages()) == 4 }, 2*time.Second, 5*time.Millisecond)
	msgs := srv.ClientMessages()
	require.True(t, msgs[0].Text)
	require.False(t, msgs[1].Text)
	require.Equal(t, []byte{1, 2, 3, 4}, msgs[1].Data)
	require.JSONEq(t, `{"commit":true,"x":1}`, string(msgs[2].Data))
	require.False(t, msgs[3].At.Before(msgs[2].At))
	srv.AssertDone()
}

func TestWSSimRuleTimesAndBinaryMatch(t *testing.T) {
	s := replay.LoadScriptString(t, `{"meta":{"vendor":"ELEVENLABS","capability":"STT","source":"x","verified":false}}
{"ws":{"dir":"on","match":{"binary":true},"send":[{"text":"{\"ack\":1}"}],"times":1}}
{"ws":{"dir":"recv","binary":true}}`)
	srv := replay.NewWSServer(t, s)
	c := dial(t, srv.URL)
	ctx := context.Background()
	require.NoError(t, c.Write(ctx, websocket.MessageBinary, []byte{1}))
	require.JSONEq(t, `{"ack":1}`, readText(t, c))
	// the rule is spent after one use, so the next binary frame reaches the recv line
	require.NoError(t, c.Write(ctx, websocket.MessageBinary, []byte{2}))
	require.Eventually(t, func() bool { return len(srv.ClientMessages()) == 2 }, 2*time.Second, 5*time.Millisecond)
	srv.AssertDone()
}

func TestWSSimUpgradeAndClose(t *testing.T) {
	s := replay.LoadScriptString(t, `{"meta":{"vendor":"CARTESIA","capability":"STT","source":"x","verified":false}}
{"ws_upgrade":{"path":"/stt/websocket","query":{"model":"m","api_key":"*"},"headers":{"X-Test":"1"}}}
{"ws":{"dir":"close","code":4001}}`)
	srv := replay.NewWSServer(t, s)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, srv.URL+"/stt/websocket?model=m&api_key=k", &websocket.DialOptions{HTTPHeader: http.Header{"X-Test": {"1"}}})
	require.NoError(t, err)
	defer c.CloseNow() //nolint:errcheck // test teardown
	_, _, err = c.Read(ctx)
	require.Equal(t, websocket.StatusCode(4001), websocket.CloseStatus(err))
	up := srv.UpgradeRequest()
	require.Equal(t, "/stt/websocket", up.Path)
	require.Equal(t, "k", up.Query.Get("api_key"))
	require.Equal(t, "1", up.Header.Get("X-Test"))
	srv.AssertDone()
}

func TestWSSimMismatchFailsTest(t *testing.T) {
	t.Run("recv json", func(t *testing.T) {
		rt := &recTB{TB: t}
		srv := replay.NewWSServer(rt, replay.LoadScriptString(t, metaLine+
			`{"ws":{"dir":"recv","json":{"a":{"b":1}}}}`))
		c := dial(t, srv.URL)
		require.NoError(t, c.Write(context.Background(), websocket.MessageText, []byte(`{"a":{"b":2}}`)))
		require.Eventually(t, func() bool { return rt.all() != "" }, 2*time.Second, 5*time.Millisecond)
		require.Contains(t, rt.all(), "line 2 (ws recv): json.a.b: want 1 got 2")
	})
	t.Run("frame type", func(t *testing.T) {
		rt := &recTB{TB: t}
		srv := replay.NewWSServer(rt, replay.LoadScriptString(t, metaLine+
			`{"ws":{"dir":"recv","binary":true,"len":3}}`))
		c := dial(t, srv.URL)
		require.NoError(t, c.Write(context.Background(), websocket.MessageBinary, []byte{1, 2}))
		require.Eventually(t, func() bool { return rt.all() != "" }, 2*time.Second, 5*time.Millisecond)
		require.Contains(t, rt.all(), "binary: want 3 bytes got 2")
	})
	t.Run("upgrade", func(t *testing.T) {
		rt := &recTB{TB: t}
		srv := replay.NewWSServer(rt, replay.LoadScriptString(t, metaLine+
			`{"ws_upgrade":{"path":"/x","query":{"model":"m"}}}`))
		dial(t, srv.URL+"/x?model=other")
		require.Contains(t, rt.all(), `line 2 (ws_upgrade): query.model: want "m" got "other"`)
	})
	t.Run("unreached lines fail AssertDone", func(t *testing.T) {
		rt := &recTB{TB: t}
		srv := replay.NewWSServer(rt, replay.LoadScriptString(t, metaLine+
			`{"ws":{"dir":"recv","json":{"a":1}}}`))
		dial(t, srv.URL)
		srv.AssertDone()
		require.Contains(t, rt.all(), "was never reached")
	})
	t.Run("no client", func(t *testing.T) {
		rt := &recTB{TB: t}
		srv := replay.NewWSServer(rt, replay.LoadScriptString(t, metaLine))
		srv.AssertDone()
		require.Contains(t, rt.all(), "no client ever connected")
	})
}

func TestWSSimShutdownReleasesHandlerOfUnfinishedScript(t *testing.T) {
	// the server cleanup must return even though the client is still connected and the script
	// is still waiting for a message (t.Run returns only after the subtest cleanups ran)
	s := replay.LoadScriptString(t, metaLine+`{"ws":{"dir":"recv","json":{"a":1}}}`)
	finished := make(chan struct{})
	var client *websocket.Conn // deliberately not closed before the server cleanup runs
	go func() {
		defer close(finished)
		t.Run("scope", func(t *testing.T) {
			srv := replay.NewWSServer(t, s)
			c, _, err := websocket.Dial(context.Background(), srv.URL, nil)
			require.NoError(t, err)
			client = c
		})
	}()
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("server shutdown hung on a connected client")
	}
	_ = client.CloseNow()
}
