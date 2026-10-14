package health_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/nabePi/kotoki-backend/internal/platform/health"
	"github.com/nabePi/kotoki-backend/internal/platform/metrics"
)

func newOps(o health.OpsOptions) *health.Ops {
	if o.Registry == nil {
		o.Registry = metrics.NewRegistry()
	}
	o.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	return health.NewOps(o)
}

func get(t *testing.T, h http.Handler, path string) (int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec.Code, strings.TrimSpace(rec.Body.String())
}

func TestHealthzAlways200(t *testing.T) {
	o := newOps(health.OpsOptions{})
	code, body := get(t, o.Handler(), "/healthz")
	if code != 200 || body != `{"status":"ok"}` {
		t.Fatalf("got %d %s", code, body)
	}
	o.SetDraining()
	code, body = get(t, o.Handler(), "/healthz")
	if code != 200 || body != `{"status":"ok"}` {
		t.Fatalf("draining: got %d %s", code, body)
	}
}

func TestReadyzStartingThenReady(t *testing.T) {
	o := newOps(health.OpsOptions{Checks: []health.Check{{Name: "postgres", Fn: func(context.Context) error { return nil }}}})
	code, body := get(t, o.Handler(), "/readyz")
	if code != 503 || body != `{"status":"starting","checks":{}}` {
		t.Fatalf("got %d %s", code, body)
	}
	if err := o.RunStartupChecks(context.Background()); err != nil {
		t.Fatal(err)
	}
	code, body = get(t, o.Handler(), "/readyz")
	if code != 200 || body != `{"status":"ready"}` {
		t.Fatalf("got %d %s", code, body)
	}
}

func TestReadyzFailedCheck(t *testing.T) {
	o := newOps(health.OpsOptions{Checks: []health.Check{
		{Name: "postgres", Fn: func(context.Context) error { return errors.New("secret-dsn boom") }},
		{Name: "redis", Fn: func(context.Context) error { return nil }},
	}})
	if err := o.RunStartupChecks(context.Background()); err == nil {
		t.Fatal("expected error")
	}
	code, body := get(t, o.Handler(), "/readyz")
	if code != 503 || !strings.Contains(body, `"postgres":"fail"`) {
		t.Fatalf("got %d %s", code, body)
	}
	if strings.Contains(body, "secret-dsn") || strings.Contains(body, "boom") {
		t.Fatalf("error text leaked: %s", body)
	}
}

func TestReadyStaysAfterDependencyFails(t *testing.T) {
	var fail atomic.Bool
	o := newOps(health.OpsOptions{Checks: []health.Check{{Name: "postgres", Fn: func(context.Context) error {
		if fail.Load() {
			return errors.New("down")
		}
		return nil
	}}}})
	if err := o.RunStartupChecks(context.Background()); err != nil {
		t.Fatal(err)
	}
	fail.Store(true)
	code, body := get(t, o.Handler(), "/readyz")
	if code != 200 || body != `{"status":"ready"}` {
		t.Fatalf("got %d %s", code, body)
	}
}

func TestDraining503(t *testing.T) {
	o := newOps(health.OpsOptions{})
	if err := o.RunStartupChecks(context.Background()); err != nil {
		t.Fatal(err)
	}
	o.SetDraining()
	code, body := get(t, o.Handler(), "/readyz")
	if code != 503 || body != `{"status":"draining"}` {
		t.Fatalf("got %d %s", code, body)
	}
}

func TestMetricsToggle(t *testing.T) {
	reg := metrics.NewRegistry()
	on := newOps(health.OpsOptions{Registry: reg, MetricsEnabled: true})
	code, body := get(t, on.Handler(), "/metrics")
	if code != 200 || !strings.Contains(body, "go_goroutines") {
		t.Fatalf("got %d", code)
	}
	off := newOps(health.OpsOptions{Registry: prometheus.NewRegistry()})
	if code, _ := get(t, off.Handler(), "/metrics"); code != 404 {
		t.Fatalf("got %d", code)
	}
}

func TestPprofToggle(t *testing.T) {
	on := newOps(health.OpsOptions{PprofEnabled: true})
	if code, _ := get(t, on.Handler(), "/debug/pprof/"); code != 200 {
		t.Fatalf("got %d", code)
	}
	off := newOps(health.OpsOptions{})
	if code, _ := get(t, off.Handler(), "/debug/pprof/"); code != 404 {
		t.Fatalf("got %d", code)
	}
}

func TestServeShutsDown(t *testing.T) {
	o := newOps(health.OpsOptions{})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- o.Serve(ctx, ln) }()
	resp, err := http.Get("http://" + ln.Addr().String() + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout")
	}
}
