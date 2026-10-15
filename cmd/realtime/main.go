// Command kotoki-realtime serves the WebSocket conversation endpoint (spec Bab 7): it wires config,
// logging, tracing, metrics, Postgres, Redis, the provider router (fake providers in M5), the
// conversation Manager and the WebSocket gateway behind the ops listener (/healthz, /readyz,
// /metrics). SIGTERM drains the pod before it exits (spec 7.12.1).
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"

	"github.com/nabePi/kotoki-backend/internal/balance"
	"github.com/nabePi/kotoki-backend/internal/content"
	"github.com/nabePi/kotoki-backend/internal/conversation"
	"github.com/nabePi/kotoki-backend/internal/platform/buildinfo"
	"github.com/nabePi/kotoki-backend/internal/platform/clock"
	"github.com/nabePi/kotoki-backend/internal/platform/config"
	"github.com/nabePi/kotoki-backend/internal/platform/health"
	"github.com/nabePi/kotoki-backend/internal/platform/idgen"
	"github.com/nabePi/kotoki-backend/internal/platform/logging"
	"github.com/nabePi/kotoki-backend/internal/platform/metrics"
	"github.com/nabePi/kotoki-backend/internal/platform/otel"
	"github.com/nabePi/kotoki-backend/internal/platform/ratelimit"
	"github.com/nabePi/kotoki-backend/internal/postgres"
	"github.com/nabePi/kotoki-backend/internal/provider"
	"github.com/nabePi/kotoki-backend/internal/redisstore"
	"github.com/nabePi/kotoki-backend/internal/session"
	"github.com/nabePi/kotoki-backend/internal/wsgateway"
)

const (
	service = "kotoki-realtime"
	// drainTimeout is how long SIGTERM waits for the sessions to hand off (spec 7.12.1: t<=30s,
	// inside terminationGracePeriodSeconds 40 minus the 5 s preStop).
	drainTimeout = 30 * time.Second
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	err := run(ctx)
	stop()
	if err != nil {
		fmt.Fprintln(os.Stderr, service+":", err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	cfg, err := config.LoadRealtime()
	if err != nil {
		return err
	}
	var level slog.Level
	if err := level.UnmarshalText([]byte(cfg.LogLevel)); err != nil {
		level = slog.LevelInfo
	}
	logger := logging.New(os.Stdout, logging.Options{
		Level: level, Format: cfg.LogFormat, Service: service,
		Version: buildinfo.Version, GitCommit: buildinfo.GitCommit, Env: cfg.AppEnv, LogTranscripts: cfg.LogTranscripts,
	})
	slog.SetDefault(logger)
	logger.InfoContext(ctx, "starting", "build", buildinfo.String())

	otelShutdown, err := otel.Setup(ctx, otel.Config{
		Endpoint: cfg.Endpoint, ServiceName: cfg.ServiceName, ResourceAttributes: cfg.ResourceAttributes,
		Version: buildinfo.Version, Env: cfg.AppEnv,
	})
	if err != nil {
		return err
	}
	defer func() { _ = otelShutdown(context.WithoutCancel(ctx)) }()

	reg := metrics.NewRegistry()
	var pool *pgxpool.Pool
	dbMetrics := metrics.NewDB(reg, func() metrics.PoolStat {
		if pool == nil {
			return metrics.PoolStat{}
		}
		return postgres.PoolStat(pool)
	})
	pool, err = postgres.NewPool(ctx, cfg.DB, postgres.PoolOptions{ServiceName: service, Metrics: dbMetrics})
	if err != nil {
		return err
	}
	defer pool.Close()
	rdb, err := redisstore.NewClient(cfg.Redis, metrics.NewRedis(reg))
	if err != nil {
		return err
	}
	defer func() { _ = rdb.Close() }()

	// podCtx outlives the signal: the podinfo writer, the TAKEOVER subscriber and the fault poller
	// keep running while the pod drains and stop after the Manager closed.
	podCtx, stopPod := context.WithCancel(context.WithoutCancel(ctx))
	defer stopPod()
	app, err := buildRealtime(podCtx, cfg, logger, pool, rdb, reg)
	if err != nil {
		return err
	}
	defer app.mgr.Close() // hard stop; a no-op after the normal shutdown below

	ops := health.NewOps(health.OpsOptions{
		Registry: reg, MetricsEnabled: cfg.MetricsEnabled, PprofEnabled: cfg.PprofEnabled, Logger: logger,
		Checks: []health.Check{
			{Name: "postgres", Fn: pool.Ping},
			{Name: "redis", Fn: func(ctx context.Context) error { return redisstore.Ping(ctx, rdb) }},
		},
		Wrap: opsWrap(app.mgr),
	})
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", cfg.OpsAddr)
	if err != nil {
		return err
	}
	// The ops listener outlives the signal context so /readyz can report "draining" while the
	// sessions hand off; it stops last.
	opsCtx, stopOps := context.WithCancel(context.WithoutCancel(ctx))
	defer stopOps()
	serveErr := make(chan error, 1)
	go func() { serveErr <- ops.Serve(opsCtx, ln) }()

	// Bind the public port before readiness flips so that a ready pod is always reachable.
	wsLn, err := (&net.ListenConfig{}).Listen(ctx, "tcp", cfg.HTTPAddr)
	if err != nil {
		return err
	}
	defer func() { _ = wsLn.Close() }()
	wsSrv := &http.Server{
		Handler: app.handler, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 120 * time.Second, MaxHeaderBytes: 16 << 10,
	}

	checkCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := ops.RunStartupChecks(checkCtx); err != nil {
		return err
	}
	wsErr := make(chan error, 1)
	go func() { wsErr <- wsSrv.Serve(wsLn) }()
	logger.InfoContext(ctx, "ready", "ops_addr", ln.Addr().String(), "http_addr", wsLn.Addr().String(), "pod", cfg.PodID)

	select {
	case <-ctx.Done():
	case err := <-wsErr:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case err := <-serveErr:
		if err == nil {
			err = errors.New("ops listener stopped unexpectedly")
		}
		return err
	}

	// Graceful shutdown (spec 7.12.1): /readyz goes 503, the public listener keeps answering so that
	// a new connection gets 503 SERVER_DRAINING, the sessions hand off (close 4100), then the
	// Manager stops and the listeners follow. A drain that runs out of time is logged, not fatal:
	// Close below is the hard stop and the sweeper settles what is left.
	bg := context.WithoutCancel(ctx)
	ops.SetDraining()
	drainCtx, stopDrain := context.WithTimeout(bg, drainTimeout)
	if err := app.mgr.Drain(drainCtx); err != nil {
		logger.WarnContext(bg, "drain did not finish", "error", err.Error(), "active_sessions", app.mgr.ActiveSessions())
	}
	stopDrain()
	app.mgr.Close()
	shutdownCtx, stopShutdown := context.WithTimeout(bg, 5*time.Second)
	defer stopShutdown()
	if err := wsSrv.Shutdown(shutdownCtx); err != nil {
		_ = wsSrv.Close()
		logger.WarnContext(bg, "public listener did not shut down in time", "error", err.Error())
	}
	stopPod()
	stopOps()
	if err := <-serveErr; err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	logger.InfoContext(bg, "stopped")
	return nil
}

// realtimeApp is the wired pod: the public handler and the Manager the shutdown drives.
type realtimeApp struct {
	handler http.Handler
	mgr     *conversation.Manager
}

// buildRealtime wires the components by hand (D-004), following cmd/api for the session side, and
// starts the Manager's background parts (TAKEOVER subscriber, podinfo loop, fault poller) on podCtx.
func buildRealtime(podCtx context.Context, cfg config.Realtime, logger *slog.Logger, pool *pgxpool.Pool, rdb redis.UniversalClient, reg *prometheus.Registry) (*realtimeApp, error) {
	ids, clk := idgen.NewV7(), clock.System{}
	store := postgres.NewStore(pool, postgres.WithIDs(ids), postgres.WithClock(clk),
		postgres.WithTimeouts(cfg.LockTimeout, cfg.StatementTimeout))

	router, rc, faults, err := buildRouter(rdb, clk, logger, provider.NewMetrics(reg))
	if err != nil {
		return nil, fmt.Errorf("provider: %w", err)
	}

	// session.Service is the single writer of sessions.status; the runtime reaches it through the
	// fenced RuntimeService facade. The realtime process never creates sessions or reads content
	// through the resolver, so those collaborators stay unset.
	sessions := postgres.NewSessionRepo(pool)
	sessionSvc := session.NewService(session.Deps{
		Store: store.SessionStore(), Reader: sessions, Balance: balance.NewService(store.BalanceStore(), clk, ids),
		Control: redisstore.NewSessionControl(rdb), Clock: clk, IDs: ids, Log: logger,
		Metrics: metrics.NewSession(reg), ServiceName: "realtime",
	})
	contentRepo := postgres.NewContentRepo(pool)
	convMetrics := conversation.NewMetrics(reg)
	mgr := conversation.NewManager(conversation.Deps{
		Cfg: cfg.RealtimeTuning, Clock: clk, IDs: ids, Log: logger, Metrics: convMetrics,
		Limits:  conversation.Limits{ReadyMax: cfg.ReadyMaxSessions, Max: cfg.MaxSessions, HardMax: cfg.HardMaxSessions},
		Session: session.NewRuntimeService(sessionSvc, sessions),
		Turns:   postgres.NewTurnRepo(pool),
		Events:  conversation.NewSessionEventWriter(postgres.NewEventRepo(pool), ids, clk),
		Store:   redisstore.NewRealtimeState(rdb, logger),
		Configs: conversation.NewConfigLoader(content.NewSessionConfigLoader(contentRepo), contentRepo, 128),
		Learner: staticLearner{}, Memory: noopMemory{}, Router: router,
		ProviderSessionGrace: rc.SessionGrace, STTFinalTimeout: rc.TimeoutSTTFinal, Build: buildinfo.Version,
	})
	mgr.StartSubscriber(podCtx)
	go mgr.PodInfoLoop(podCtx)
	go faults.Run(podCtx)

	gateway := &wsgateway.Handler{
		Tuning: cfg.RealtimeTuning, Origins: cfg.WSAllowedOrigins, AllowEmptyOrigin: cfg.WSAllowEmptyOrigin,
		TrustedProxies: cfg.TrustedProxyPrefixes(),
		Limiter:        redisstore.NewRateLimiter(rdb, ratelimit.NewLocal(clk, 100000), clk, logger, metrics.NewRateLimit(reg)),
		Tickets:        redisstore.NewTicketStore(rdb, clk),
		Mgr:            mgr, Log: logger, Metrics: convMetrics, IDs: ids, Clock: clk,
	}
	mux := http.NewServeMux()
	mux.Handle("GET /api/v1/realtime", gateway)
	return &realtimeApp{handler: mux, mgr: mgr}, nil
}
