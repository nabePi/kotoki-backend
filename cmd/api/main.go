// Command kotoki-api serves the public REST API: it wires config, logging, tracing, metrics,
// Postgres and Redis behind the ops listener and mounts the auth, balance, admin, content and
// session handlers (the realtime and worker features live in their own binaries).
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

	"github.com/nabePi/kotoki-backend/internal/admin"
	"github.com/nabePi/kotoki-backend/internal/auth"
	"github.com/nabePi/kotoki-backend/internal/balance"
	"github.com/nabePi/kotoki-backend/internal/content"
	"github.com/nabePi/kotoki-backend/internal/httpapi"
	"github.com/nabePi/kotoki-backend/internal/mailer"
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
	"github.com/nabePi/kotoki-backend/internal/redisstore"
	"github.com/nabePi/kotoki-backend/internal/session"
)

const service = "kotoki-api"

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
	cfg, err := config.LoadAPI()
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
	// The ops listener outlives the signal context so /readyz can report "draining"
	// while the public listener finishes in-flight requests; it stops last.
	opsCtx, stopOps := context.WithCancel(context.WithoutCancel(ctx))
	defer stopOps()
	serveErr := make(chan error, 1)
	go func() { serveErr <- ops.Serve(opsCtx, ln) }()

	router, dispatcher, err := buildAPI(ctx, cfg, logger, pool, rdb, reg)
	if err != nil {
		return err
	}
	// Bounded by the shutdown budget: with SMTP down an unbounded drain would outlive SIGKILL.
	defer func() {
		closeCtx, stopClose := context.WithTimeout(context.WithoutCancel(ctx), cfg.ShutdownTimeout)
		defer stopClose()
		_ = dispatcher.Close(closeCtx)
	}()
	// Bind the public port before readiness flips so that a ready pod is always reachable;
	// connections queue in the backlog until Serve starts right after the startup checks.
	apiLn, err := (&net.ListenConfig{}).Listen(ctx, "tcp", cfg.HTTPAddr)
	if err != nil {
		return err
	}
	defer func() { _ = apiLn.Close() }()
	apiSrv := &http.Server{
		Handler: router, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second,
		WriteTimeout: 30 * time.Second, IdleTimeout: 120 * time.Second, MaxHeaderBytes: 16 << 10,
	}

	checkCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := ops.RunStartupChecks(checkCtx); err != nil {
		return err
	}
	apiErr := make(chan error, 1)
	go func() { apiErr <- apiSrv.Serve(apiLn) }()
	logger.InfoContext(ctx, "ready", "ops_addr", ln.Addr().String(), "http_addr", apiLn.Addr().String())

	select {
	case <-ctx.Done():
	case err := <-apiErr:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case err := <-serveErr:
		if err == nil {
			err = errors.New("ops listener stopped unexpectedly")
		}
		return err
	}
	ops.SetDraining()
	shutdownCtx, stopShutdown := context.WithTimeout(context.WithoutCancel(ctx), cfg.ShutdownTimeout)
	defer stopShutdown()
	if err := apiSrv.Shutdown(shutdownCtx); err != nil {
		return err
	}
	stopOps()
	if err := <-serveErr; err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	logger.InfoContext(context.WithoutCancel(ctx), "stopped")
	return nil
}

// buildAPI wires the components by hand (D-004) and returns the public router plus the mail
// dispatcher the caller must close on shutdown.
func buildAPI(ctx context.Context, cfg config.API, logger *slog.Logger, pool *pgxpool.Pool, rdb redis.UniversalClient, reg *prometheus.Registry) (http.Handler, *auth.MailDispatcher, error) {
	ids, clk := idgen.NewV7(), clock.System{}
	store := postgres.NewStore(pool, postgres.WithIDs(ids), postgres.WithClock(clk),
		postgres.WithTimeouts(cfg.LockTimeout, cfg.StatementTimeout))

	secret, err := cfg.JWTSecretBytes()
	if err != nil {
		return nil, nil, err
	}
	previous, err := cfg.JWTPreviousSecretBytes()
	if err != nil {
		return nil, nil, err
	}
	tokens, err := auth.NewJWTIssuer(auth.JWTConfig{
		Secret: secret, PreviousSecret: previous, Issuer: cfg.JWTIssuer, Audience: cfg.JWTAudience, TTL: cfg.AccessTokenTTL,
	}, ids)
	if err != nil {
		return nil, nil, err
	}
	hasher := auth.NewArgon2Hasher(auth.Argon2Config{
		MemoryKiB: cfg.Argon2.MemoryKiB, Iterations: cfg.Argon2.Iterations, Parallelism: cfg.Argon2.Parallelism,
		MaxConcurrent: cfg.Argon2.MaxConcurrent, MaxWait: 2 * time.Second,
	})
	sender, err := mailer.New(mailer.Config{
		Mode: cfg.Mail.Mode, AppEnv: cfg.AppEnv, From: cfg.Mail.From, SMTPHost: cfg.Mail.SMTPHost, SMTPPort: cfg.Mail.SMTPPort,
		SMTPUsername: cfg.Mail.SMTPUsername, SMTPPassword: cfg.Mail.SMTPPassword.Reveal(),
	}, logger)
	if err != nil {
		return nil, nil, err
	}
	dispatcher := auth.NewMailDispatcher(sender, logger, auth.DispatcherOptions{}) //nolint:contextcheck // the dispatcher owns its own lifecycle

	balanceSvc := balance.NewService(store.BalanceStore(), clk, ids)
	adminSvc := admin.NewService(store.AdminStore(), balanceSvc, clk, ids, logger)
	authSvc, err := auth.NewService(store.AuthStore(), hasher, tokens, dispatcher, clk, ids, logger, metrics.NewAuth(reg), auth.Config{ //nolint:contextcheck // wiring only: the dispatcher owns its own lifecycle
		TermsVersion: cfg.PrivacyPolicyVersion, InitialBalanceSeconds: cfg.InitialBalanceSeconds, AccessTTL: cfg.AccessTokenTTL,
		RefreshTTL: cfg.RefreshTokenTTL, RefreshReuseGrace: cfg.RefreshReuseGrace, ResetTTL: cfg.PasswordResetTTL,
		ResetURLBase: cfg.PasswordResetURLBase,
	})
	if err != nil {
		return nil, nil, err
	}
	// admin.Service doubles as the AccountDeleter behind DELETE /me (spec 3.2).
	userSvc := auth.NewUserService(store.AuthStore(), hasher, adminSvc, clk, logger, cfg.PrivacyPolicyVersion)

	contentSvcs, err := buildContent(ctx, cfg, logger, pool, store, reg, ids, clk)
	if err != nil {
		return nil, nil, err
	}
	sessionSvc := session.NewService(session.Deps{
		Store: store.SessionStore(), Reader: postgres.NewSessionRepo(pool), Balance: balanceSvc, Resolver: contentSvcs.resolver,
		Adaptive: session.NeutralAdaptiveProvider{}, Users: userGate{users: userSvc}, OpeningPrompts: openingPromptSource{repo: contentSvcs.cached},
		Control: redisstore.NewSessionControl(rdb), Clock: clk, IDs: ids, Log: logger, TermsVersion: userSvc.CurrentTermsVersion(),
		Metrics: metrics.NewSession(reg), ServiceName: "api",
	})
	// the admin account-deletion path cancels a RESERVED session through the session service
	store.SetSessionCanceller(sessionSvc)
	sessionQuery := session.NewQueryService(postgres.NewSessionViewRepo(pool), postgres.NewTurnRepo(pool), clk, cfg.RetentionTranscriptDays)
	adminSessions := session.NewAdminQueryService(postgres.NewSessionAdminRepo(pool), clk, cfg.RetentionTranscriptDays)
	tickets := auth.NewTicketService(store.AuthStore(), postgres.NewTicketSessionReader(pool), redisstore.NewTicketStore(rdb, clk), clk,
		auth.TicketConfig{TTL: cfg.WSTicketTTL, RealtimeURL: cfg.RealtimePublicURL})

	router := httpapi.NewRouter(httpapi.Deps{
		Log: logger, Clock: clk, Tokens: tokens, Metrics: metrics.NewHTTP(reg, service),
		Limiter: redisstore.NewRateLimiter(rdb, ratelimit.NewLocal(clk, 100000), clk, logger, metrics.NewRateLimit(reg)),
		Cfg: httpapi.Config{
			CORSOrigins: cfg.CORSAllowedOrigins, TrustedProxies: cfg.TrustedProxyPrefixes(), BodyLimit: cfg.HTTPMaxBodyBytes,
			Cookie: httpapi.CookieConfig{Secure: cfg.RefreshCookieSecure, Domain: cfg.RefreshCookieDomain, MaxAge: cfg.RefreshTokenTTL},
		},
		Auth: authSvc, Users: userSvc, Balance: balanceSvc, Admin: adminSvc, AdminGate: adminSvc,
		Content: contentSvcs.catalog, ContentAdmin: contentSvcs.admin,
		Session: sessionSvc, SessionQuery: sessionQuery, Tickets: tickets,
		AdminSessions: adminSessions, AdminAudit: adminSvc,
	})
	return router, dispatcher, nil
}

// contentServices are the content components the api binary mounts.
type contentServices struct {
	cached   content.Repository
	resolver content.Resolver
	catalog  *content.CatalogService
	admin    *content.AdminService
}

// buildContent wires the content side: the in-process cache over Postgres (30 s for live reads, spec
// 6.5), the resolver, the catalog and the admin lifecycle, and validates the whole catalog once.
// Broken content never stops the process: it only hides the entity (spec 6.10.3).
func buildContent(ctx context.Context, cfg config.API, logger *slog.Logger, pool *pgxpool.Pool, store *postgres.Store, reg *prometheus.Registry,
	ids idgen.Generator, clk clock.Clock) (contentServices, error) {
	rc, err := loadProviderSettings()
	if err != nil {
		return contentServices{}, fmt.Errorf("provider settings: %w", err)
	}
	cache := content.NewCachedRepository(postgres.NewContentRepo(pool), cfg.CacheTTL(), clk)
	observer := metrics.NewContent(reg)
	vcfg := validatorConfig(rc)
	content.ReportCatalog(ctx, content.NewValidator(cache, vcfg), observer, logger)
	return contentServices{
		cached:   cache,
		resolver: content.NewResolver(cache, resolverConfig(rc), observer),
		catalog:  content.NewCatalogService(cache, resolverConfig(rc)),
		admin: content.NewAdminService(content.AdminDeps{
			Store: store.ContentStore(), Validator: vcfg, Clock: clk, IDs: ids, Invalidator: cache.(content.Invalidator), RequestID: httpapi.RequestIDFrom,
		}),
	}, nil
}
