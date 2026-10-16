package provider_test

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/provider"
	"github.com/nabePi/kotoki-backend/internal/provider/cartesia"
	"github.com/nabePi/kotoki-backend/internal/provider/elevenlabs"
	"github.com/nabePi/kotoki-backend/internal/provider/httpx"
	"github.com/nabePi/kotoki-backend/internal/provider/openai"
	"github.com/nabePi/kotoki-backend/internal/testutil/replay"
)

// countingProxy forwards to the (ws://) simulator and counts WebSocket handshakes, so a test can
// tell a second connection to the SAME vendor from a replay on the first one.
func countingProxy(t *testing.T, target string) (wsURL string, handshakes *atomic.Int32) {
	t.Helper()
	u, err := url.Parse("http" + target[len("ws"):])
	require.NoError(t, err)
	rp := httputil.NewSingleHostReverseProxy(u)
	handshakes = &atomic.Int32{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handshakes.Add(1)
		rp.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return "ws" + srv.URL[len("http"):], handshakes
}

type rotationAdapter struct {
	vendor string
	name   provider.Name
	flag   func(*provider.RouterConfig)
	open   func(wsURL string) (provider.STTProvider, error)
	// rotate are the vendor codes the adapter maps to a rotation signal; guess marks the ones
	// that are NOT documented by the vendor (invented from the adapter's best reading).
	rotate []rotationCode
	// forward are vendor codes of ordinary failures: they must reach the runtime, not rotate.
	forward []string
}

type rotationCode struct {
	code  string
	guess string // empty = documented message_type; otherwise why it is a guess
}

func rotationAdapters() []rotationAdapter {
	setSTT := func(f func(c *provider.RouterConfig)) func(*provider.RouterConfig) {
		return func(c *provider.RouterConfig) {
			c.OpenAISTTEnabled, c.ElevenLabsSTTEnabled, c.CartesiaSTTEnabled = false, false, false
			f(c)
		}
	}
	return []rotationAdapter{
		{vendor: "elevenlabs", name: provider.ElevenLabs,
			flag: setSTT(func(c *provider.RouterConfig) { c.ElevenLabsSTTEnabled = true }),
			open: func(u string) (provider.STTProvider, error) {
				return elevenlabs.NewSTT(elevenlabs.Config{APIKey: "xi-test-key", WSBaseURL: u, STTModel: "m_stt", SyntheticFinalDelay: 30 * time.Millisecond}, httpx.NewClient(), nil)
			},
			rotate: []rotationCode{
				{code: "session_time_limit_exceeded"}, // documented message_type; the limit itself is not
				{code: "insufficient_audio_activity"}, // documented message_type (idle close)
			},
			forward: []string{"auth_error", "quota_exceeded", "rate_limited", "transcriber_error"}},
		{vendor: "openai", name: provider.OpenAI,
			flag: setSTT(func(c *provider.RouterConfig) { c.OpenAISTTEnabled = true }),
			open: func(u string) (provider.STTProvider, error) {
				return openai.NewSTT(openai.Config{APIKey: "sk-test", WSBaseURL: u, STTModel: "m_stt", SyntheticFinalDelay: 30 * time.Millisecond}, httpx.NewClient(), nil)
			},
			rotate: []rotationCode{
				{code: "session_expired", guess: "the Realtime docs name no session-duration error code (M9-T4); session_expired is the adapter's best reading"},
			},
			forward: []string{"invalid_api_key", "rate_limit_exceeded", "insufficient_quota", "server_error"}},
		{vendor: "cartesia", name: provider.Cartesia,
			flag: setSTT(func(c *provider.RouterConfig) { c.CartesiaSTTEnabled = true }),
			open: func(u string) (provider.STTProvider, error) {
				return cartesia.NewSTT(cartesia.Config{APIKey: "ct-test-key", WSBaseURL: u, STTModel: "m_ink", Version: "v1"}, httpx.NewClient(), nil)
			},
			rotate: []rotationCode{
				{code: "session_time_limit_exceeded", guess: "the Cartesia STT docs state no session limit and no error_code for one (M9-T10); the code reuses the router's"},
			},
			forward: []string{"quota_exceeded", "model_not_found", "internal_error"}},
	}
}

func rotationScript(t *testing.T, vendor, code string) *replay.Script {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(replay.RecordingsDir(t, vendor), "stt_rotation.jsonl"))
	require.NoError(t, err)
	s, err := replay.ParseScript(bytes.ReplaceAll(raw, []byte("__CODE__"), []byte(code)))
	require.NoError(t, err)
	return s
}

// openRouted opens an STT session through a Router whose only STT adapter is the real one, against a
// simulator that, on the first audio, sends the vendor error `code` once.
func openRouted(t *testing.T, a rotationAdapter, code string) (*provider.Router, *testEnv, provider.RoutedSTTSession, *atomic.Int32) {
	t.Helper()
	srv := replay.NewWSServer(t, rotationScript(t, a.vendor, code))
	wsURL, handshakes := countingProxy(t, srv.URL)
	stt, err := a.open(wsURL)
	require.NoError(t, err)
	r, env := newTestRouter(t, withSTT(stt), withCfg(a.flag))
	s, err := r.OpenSTT(context.Background(), provider.STTSessionConfig{Language: "ja", SampleRate: provider.STTSampleRate})
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	require.EqualValues(t, 1, handshakes.Load())
	return r, env, s, handshakes
}

// TestAdapterRotationSignalMatchesRouter drives the REAL STT adapters through the M4 router: the
// vendor code each adapter maps for "session duration/idle limit" must make rotatingSTT reconnect
// to the same vendor and replay (spec 8.5.8); any other vendor error must reach the runtime.
// Codes marked GUESS are not documented by the vendor: they are the adapter's best reading and
// must be re-checked against a real recording (Task 16) before M9 is declared done.
func TestAdapterRotationSignalMatchesRouter(t *testing.T) {
	for _, a := range rotationAdapters() {
		for _, rc := range a.rotate {
			name := string(a.name) + "/" + rc.code
			if rc.guess != "" {
				name += "(GUESS)"
			}
			t.Run(name, func(t *testing.T) {
				if rc.guess != "" {
					t.Logf("GUESS: vendor code %q for %s is unverified: %s", rc.code, a.name, rc.guess)
				}
				r, env, s, handshakes := openRouted(t, a, rc.code)
				const sentMs = 1000
				for i := 0; i < sentMs/20; i++ {
					require.NoError(t, s.SendAudio(make([]byte, 640)))
				}
				require.Eventually(t, func() bool { return s.Stats().Reconnects == 1 }, 3*time.Second, 5*time.Millisecond,
					"the adapter's %q must be recognised by rotatingSTT as a rotation trigger", rc.code)
				require.EqualValues(t, 2, handshakes.Load(), "a second connection to the same vendor")
				require.Equal(t, a.name, s.Provider(), "no other vendor")
				require.Equal(t, provider.BreakerClosed, r.BreakerForTest(a.name, provider.CapSTT).State(), "a rotation is not a failure")
				require.Equal(t, 1.0, counter(t, env.Reg, "kotoki_stt_rotations_total", map[string]string{"provider": string(a.name)}))
				require.Eventually(t, func() bool { return s.Stats().AudioMsSent >= sentMs+20 }, 3*time.Second, 5*time.Millisecond,
					"the rotation buffer is replayed to the new connection")
				noEvent(t, s, 100*time.Millisecond) // neither ERROR nor CLOSED reaches the runtime
				require.NoError(t, s.Finalize())
				for {
					ev := nextEv(t, s)
					require.NotEqual(t, provider.STTError, ev.Type)
					require.NotEqual(t, provider.STTClosed, ev.Type)
					if ev.Type == provider.STTFinal && ev.FromFinalize {
						require.Equal(t, a.name, ev.Provider)
						break
					}
				}
			})
		}
		for _, code := range a.forward {
			t.Run(string(a.name)+"/"+code+"/forwarded", func(t *testing.T) {
				_, _, s, handshakes := openRouted(t, a, code)
				for i := 0; i < 50; i++ {
					_ = s.SendAudio(make([]byte, 640))
				}
				ev := nextEv(t, s)
				require.Equal(t, provider.STTError, ev.Type, "an ordinary vendor error is forwarded to the runtime")
				require.Equal(t, code, ev.Err.VendorCode)
				require.Equal(t, provider.STTClosed, nextEv(t, s).Type)
				require.EqualValues(t, 1, handshakes.Load(), "no reconnect")
				require.EqualValues(t, 0, s.Stats().Reconnects)
			})
		}
	}
}
