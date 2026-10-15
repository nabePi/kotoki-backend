// Command kotoki-realtime is the M1 skeleton: it wires config, logging, tracing, metrics,
// Postgres and Redis behind the ops listener. Feature handlers arrive in later milestones.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/nabePi/kotoki-backend/internal/platform/buildinfo"
	"github.com/nabePi/kotoki-backend/internal/platform/config"
	"github.com/nabePi/kotoki-backend/internal/platform/health"
	"github.com/nabePi/kotoki-backend/internal/platform/logging"
	"github.com/nabePi/kotoki-backend/internal/platform/metrics"
	"github.com/nabePi/kotoki-backend/internal/platform/otel"
	"github.com/nabePi/kotoki-backend/internal/postgres"
	"github.com/nabePi/kotoki-backend/internal/redisstore"
)

const service = "kotoki-realtime"

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

	ops := health.NewOps(health.OpsOptions{
		Registry: reg, MetricsEnabled: cfg.MetricsEnabled, PprofEnabled: cfg.PprofEnabled, Logger: logger,
		Checks: []health.Check{
			{Name: "postgres", Fn: pool.Ping},
			{Name: "redis", Fn: func(ctx context.Context) error { return redisstore.Ping(ctx, rdb) }},
		},
	})
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", cfg.OpsAddr)
	if err != nil {
		return err
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- ops.Serve(ctx, ln) }()

	checkCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := ops.RunStartupChecks(checkCtx); err != nil {
		return err
	}
	logger.InfoContext(ctx, "ready", "ops_addr", ln.Addr().String())

	<-ctx.Done()
	ops.SetDraining()
	if err := <-serveErr; err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	logger.InfoContext(context.WithoutCancel(ctx), "stopped")
	return nil
}
