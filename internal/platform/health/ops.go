// Package health serves the ops listener: liveness, readiness, metrics, pprof.
package health

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/pprof"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/nabePi/kotoki-backend/internal/platform/metrics"
)

// Check is a startup dependency check.
type Check struct {
	Name string
	Fn   func(ctx context.Context) error
}

// OpsOptions configures the ops listener.
type OpsOptions struct {
	Registry       *prometheus.Registry
	MetricsEnabled bool
	PprofEnabled   bool
	Checks         []Check
	// StartupBackoff is the first wait between failed startup rounds (default 200 ms,
	// doubling up to 2 s).
	StartupBackoff time.Duration
	Logger         *slog.Logger
}

// Ops is the ops HTTP surface. /readyz is "starting" until all startup checks
// succeed once; later dependency failures never make it unready. Providers are
// never called.
type Ops struct {
	opts     OpsOptions
	ready    atomic.Bool
	draining atomic.Bool

	mu      sync.Mutex
	results map[string]string
}

// NewOps builds an Ops.
func NewOps(o OpsOptions) *Ops {
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	return &Ops{opts: o, results: map[string]string{}}
}

// RunStartupChecks runs every check; all succeeding marks the service ready. Failed
// checks are retried with backoff until ctx ends, so a dependency that comes up a few
// seconds late does not crash-loop the process.
func (o *Ops) RunStartupChecks(ctx context.Context) error {
	wait := o.opts.StartupBackoff
	if wait <= 0 {
		wait = 200 * time.Millisecond
	}
	for {
		err := o.runChecksOnce(ctx)
		if err == nil {
			o.ready.Store(true)
			return nil
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(wait):
		}
		wait = min(wait*2, 2*time.Second)
	}
}

func (o *Ops) runChecksOnce(ctx context.Context) error {
	res := make(map[string]string, len(o.opts.Checks))
	var errs []error
	for _, c := range o.opts.Checks {
		if err := c.Fn(ctx); err != nil {
			res[c.Name] = "fail"
			o.opts.Logger.WarnContext(ctx, "startup check failed", "check", c.Name, "error", err.Error())
			errs = append(errs, errors.New("startup check "+c.Name+" failed"))
			continue
		}
		res[c.Name] = "ok"
	}
	o.mu.Lock()
	o.results = res
	o.mu.Unlock()
	return errors.Join(errs...)
}

// SetDraining makes /readyz return 503 draining.
func (o *Ops) SetDraining() { o.draining.Store(true) }

type startingBody struct {
	Status string            `json:"status"`
	Checks map[string]string `json:"checks"`
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// Handler returns the ops mux.
func (o *Ops) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		switch {
		case o.draining.Load():
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "draining"})
		case o.ready.Load():
			writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
		default:
			o.mu.Lock()
			checks := make(map[string]string, len(o.results))
			for k, v := range o.results {
				checks[k] = v
			}
			o.mu.Unlock()
			writeJSON(w, http.StatusServiceUnavailable, startingBody{Status: "starting", Checks: checks})
		}
	})
	if o.opts.MetricsEnabled && o.opts.Registry != nil {
		mux.Handle("GET /metrics", metrics.Handler(o.opts.Registry))
	}
	if o.opts.PprofEnabled {
		mux.HandleFunc("/debug/pprof/", pprof.Index)
		mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
		mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
		mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
		mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	}
	return mux
}

// Serve serves on ln until ctx is done, then shuts down within 5 s. Normal
// shutdown returns nil.
func (o *Ops) Serve(ctx context.Context, ln net.Listener) error {
	srv := &http.Server{Handler: o.Handler(), ReadHeaderTimeout: 5 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	select {
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
	}
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(sctx); err != nil {
		return err
	}
	if err := <-errc; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
