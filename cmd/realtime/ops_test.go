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

func TestRealtimeRejectsRealModeInM5(t *testing.T) {
	// M9 inverts this test: real mode is accepted once the vendor adapters exist.
	for _, mode := range []string{"real", ""} { // PROVIDER_MODE defaults to real
		t.Run("mode="+mode, func(t *testing.T) {
			t.Setenv("PROVIDER_MODE", mode)
			rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"}) // never dialled: the build fails first
			t.Cleanup(func() { _ = rdb.Close() })
			_, _, _, err := buildRouter(rdb, clock.System{}, nil, provider.NewMetrics(nil))
			require.ErrorIs(t, err, providerwire.ErrRealModeUnavailable)
			require.Contains(t, err.Error(), "M9")
		})
	}
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
