package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/cmd/internal/providerwire"
	"github.com/nabePi/kotoki-backend/internal/conversation"
	"github.com/nabePi/kotoki-backend/internal/platform/clock"
	"github.com/nabePi/kotoki-backend/internal/platform/health"
	"github.com/nabePi/kotoki-backend/internal/provider"
	"github.com/nabePi/kotoki-backend/internal/testutil"
	"github.com/nabePi/kotoki-backend/internal/testutil/rtharness"
)

type stubPod struct{ ready, draining bool }

func (s stubPod) Ready() bool    { return s.ready }
func (s stubPod) Draining() bool { return s.draining }

// readyOps is a health.Ops past its startup checks behind the realtime wrapper.
func readyOps(t *testing.T, pod podState) http.Handler {
	t.Helper()
	ops := health.NewOps(health.OpsOptions{Wrap: opsWrap(pod)})
	require.NoError(t, ops.RunStartupChecks(t.Context()))
	return opsWrap(pod)(ops.Handler())
}

func probe(t *testing.T, h http.Handler, path string) (int, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, http.NoBody))
	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body), rec.Body.String())
	return rec.Code, body
}

func TestReadyzThresholds(t *testing.T) {
	for _, tc := range []struct {
		name   string
		pod    stubPod
		code   int
		status string
	}{
		{"ready", stubPod{ready: true}, 200, "ready"},
		{"at the ready threshold", stubPod{ready: false}, 503, "at_capacity"},
		{"draining", stubPod{ready: false, draining: true}, 503, "draining"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, body := probe(t, readyOps(t, tc.pod), "/readyz")
			require.Equal(t, tc.code, code)
			require.Equal(t, tc.status, body["status"])
		})
	}
}

func TestOnlyReadyzIsGatedByThePod(t *testing.T) {
	h := readyOps(t, stubPod{ready: false})
	code, body := probe(t, h, "/healthz")
	require.Equal(t, 200, code, "liveness does not depend on capacity")
	require.Equal(t, "ok", body["status"])
}

func TestReadyzFollowsTheManager(t *testing.T) {
	testutil.NoGoroutineLeak(t, "github.com/nabePi/kotoki-backend/cmd/realtime")
	harness := rtharness.New(t, rtharness.WithLimits(conversation.Limits{ReadyMax: 1, Max: 2, HardMax: 3}))
	h := readyOps(t, harness.Mgr)
	code, body := probe(t, h, "/readyz")
	require.Equal(t, 200, code)
	require.Equal(t, "ready", body["status"])

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	require.NoError(t, harness.Mgr.Drain(ctx))
	code, body = probe(t, h, "/readyz")
	require.Equal(t, 503, code)
	require.Equal(t, "draining", body["status"])
}

func TestReadyzStillStartsBeforeTheChecksPass(t *testing.T) {
	ops := health.NewOps(health.OpsOptions{Checks: []health.Check{{Name: "x", Fn: func(context.Context) error { return context.DeadlineExceeded }}}})
	code, body := probe(t, opsWrap(stubPod{ready: true})(ops.Handler()), "/readyz")
	require.Equal(t, 503, code)
	require.Equal(t, "starting", body["status"])
}

// realEnv sets a PROVIDER_MODE=real environment: the D-017 defaults, every vendor key either a fake
// value (withKeys) or empty. No vendor is ever called: building adapters does not dial.
func realEnv(t *testing.T, mode string, withKeys bool) {
	t.Helper()
	t.Setenv("PROVIDER_MODE", mode)
	for _, k := range []string{"PROVIDER_MODE_STT", "PROVIDER_MODE_LLM", "PROVIDER_MODE_TTS", "PROVIDER_MODE_EVALUATION", "PROVIDER_MODE_EMBEDDING"} {
		t.Setenv(k, "")
	}
	for _, k := range []string{"OPENAI_API_KEY", "DEEPSEEK_API_KEY", "ELEVENLABS_API_KEY", "CARTESIA_API_KEY"} {
		v := ""
		if withKeys {
			v = "fake-" + k
		}
		t.Setenv(k, v)
	}
}

func TestRealtimeAcceptsRealModeWithKeys(t *testing.T) {
	for _, mode := range []string{"real", ""} { // PROVIDER_MODE defaults to real
		t.Run("mode="+mode, func(t *testing.T) {
			realEnv(t, mode, true)
			require.NoError(t, validateProviders())
			rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"}) // never dialled by the build
			t.Cleanup(func() { _ = rdb.Close() })
			router, rc, _, err := buildRouter(rdb, clock.System{}, nil, provider.NewMetrics(nil))
			require.NoError(t, err)
			require.NotNil(t, router)
			require.Equal(t, provider.ModeReal, rc.ModeFor(provider.CapSTT))
			require.Equal(t, provider.ModeReal, rc.ModeFor(provider.CapTTS))
		})
	}
}

func TestRealtimeRejectsRealModeWithoutKeys(t *testing.T) {
	realEnv(t, "real", false)
	err := validateProviders()
	require.ErrorContains(t, err, "OPENAI_API_KEY")
	require.ErrorContains(t, err, "ELEVENLABS_API_KEY")
	require.ErrorContains(t, err, "CARTESIA_API_KEY")
	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"}) // never dialled: the build fails first
	t.Cleanup(func() { _ = rdb.Close() })
	_, _, _, err = buildRouter(rdb, clock.System{}, nil, provider.NewMetrics(nil))
	require.ErrorContains(t, err, "OPENAI_API_KEY")
}

func TestRealtimeRejectsTwoSTTEnabled(t *testing.T) {
	realEnv(t, "real", true)
	t.Setenv("OPENAI_STT_ENABLED", "true")
	t.Setenv("CARTESIA_STT_ENABLED", "true")
	err := validateProviders()
	require.ErrorContains(t, err, "OPENAI_STT_ENABLED")
	require.ErrorContains(t, err, "ELEVENLABS_STT_ENABLED")
	require.ErrorContains(t, err, "CARTESIA_STT_ENABLED")
	require.NotContains(t, err.Error(), "fake-OPENAI_API_KEY", "key values never appear in a message")
}

func TestVoiceCheckConfigFromSwitches(t *testing.T) {
	realEnv(t, "real", true)
	rc, _, err := providerwire.ParseConfigs()
	require.NoError(t, err)
	v := voiceCheckConfig(rc, nil)
	require.True(t, v.RealTTS)
	require.Equal(t, []string{"CARTESIA", "ELEVENLABS"}, v.Vendors)
	rc.CartesiaTTSEnabled = false
	require.Equal(t, []string{"ELEVENLABS"}, voiceCheckConfig(rc, nil).Vendors)
	rc.ModeTTS = "fake"
	require.False(t, voiceCheckConfig(rc, nil).RealTTS)
}

func TestRealtimeFakeModeDoesNotValidateRealFlags(t *testing.T) {
	realEnv(t, "fake", false)
	t.Setenv("OPENAI_STT_ENABLED", "false") // would be "zero STT" in real mode
	require.NoError(t, validateProviders())
}

func TestBuildRouterInFakeMode(t *testing.T) {
	t.Setenv("PROVIDER_MODE", "fake")
	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
	t.Cleanup(func() { _ = rdb.Close() })
	router, rc, faults, err := buildRouter(rdb, clock.System{}, nil, provider.NewMetrics(nil))
	require.NoError(t, err)
	require.NotNil(t, router)
	require.NotNil(t, faults)
	require.Equal(t, "fake", rc.Mode)
}
