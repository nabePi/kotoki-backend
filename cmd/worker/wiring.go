package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	amqp091 "github.com/rabbitmq/amqp091-go"
	"github.com/redis/go-redis/v9"

	"github.com/nabePi/kotoki-backend/internal/amqp"
	"github.com/nabePi/kotoki-backend/internal/balance"
	"github.com/nabePi/kotoki-backend/internal/evaluation"
	"github.com/nabePi/kotoki-backend/internal/jobs"
	"github.com/nabePi/kotoki-backend/internal/learner"
	"github.com/nabePi/kotoki-backend/internal/memory"
	"github.com/nabePi/kotoki-backend/internal/platform/buildinfo"
	"github.com/nabePi/kotoki-backend/internal/platform/clock"
	"github.com/nabePi/kotoki-backend/internal/platform/config"
	"github.com/nabePi/kotoki-backend/internal/platform/health"
	"github.com/nabePi/kotoki-backend/internal/platform/idgen"
	"github.com/nabePi/kotoki-backend/internal/platform/metrics"
	"github.com/nabePi/kotoki-backend/internal/platform/otel"
	"github.com/nabePi/kotoki-backend/internal/postgres"
	"github.com/nabePi/kotoki-backend/internal/provider"
	"github.com/nabePi/kotoki-backend/internal/redisstore"
	"github.com/nabePi/kotoki-backend/internal/session"
	"github.com/nabePi/kotoki-backend/internal/summary"
)

// consumerFlags are the --handlers values that run a queue consumer (spec 9.8).
var consumerFlags = []string{"evaluation", "memory", "learner", "summary"}

// coreFlags are the --handlers values without a consumer: the publisher and the job groups.
var coreFlags = []string{"outbox", "sweeper", "cleanup"}

// HandlerSet is what a --handlers list turns on (spec 9.8): the outbox publisher and the scheduled
// jobs belong to the core flags, the other four flags are queue consumers.
type HandlerSet struct {
	Publisher bool
	Jobs      []string // job names, in the order of the table of 9.9.1
	Consumers []string // consumer flags, in the order given, without duplicates
}

// NeedsAMQP reports whether the process talks to RabbitMQ at all: the publisher publishes, a
// consumer consumes and republishes retries. Jobs alone need no broker.
func (h HandlerSet) NeedsAMQP() bool { return h.Publisher || len(h.Consumers) > 0 }

// handlerSet is a pure function of the flag list; it builds nothing.
func handlerSet(flags []string) HandlerSet {
	var h HandlerSet
	h.Publisher = slices.Contains(flags, "outbox")
	for _, s := range jobs.SpecsForFlags(flags) {
		h.Jobs = append(h.Jobs, s.Name)
	}
	for _, f := range flags {
		if slices.Contains(consumerFlags, f) && !slices.Contains(h.Consumers, f) {
			h.Consumers = append(h.Consumers, f)
		}
	}
	return h
}

// consumerDeps are the collaborators a consumer is built from (spec 9.7).
type consumerDeps struct {
	Conn      *amqp.Conn
	Pub       amqp.Publisher
	Processed amqp.ProcessedChecker
	Metrics   *amqp.Metrics
	Pool      *pgxpool.Pool
	// Store is the transactional store of the handlers (configured with the worker lock and
	// statement timeouts); built once in serve.
	Store *postgres.Store
	Redis redis.UniversalClient
	Log   *slog.Logger
	Clock clock.Clock
	// Reg receives the feature metrics of the handlers (each is registered once per process).
	Reg prometheus.Registerer
	// Router is the provider router of the worker; evaluation, summary and memory require it
	// (memory extracts through its evaluation vendor and embeds through its embedding adapter).
	Router provider.AsyncRouter
	// MemVer moves the memory version of a user after an extraction committed; nil means the Redis
	// bumper over Redis.
	MemVer memory.MemVer
	// EvalCfg and SummaryCfg are config.Worker.Evaluation/Summary mapped by evaluationConfig/summaryConfig.
	EvalCfg    evaluation.Config
	SummaryCfg summary.Config
}

// buildConsumers builds the consumer of every feature flag in flags. Flags that are not consumers
// (outbox, sweeper, cleanup) are ignored.
func buildConsumers(flags []string, d consumerDeps) ([]*amqp.Consumer, error) {
	for _, f := range flags {
		if !slices.Contains(consumerFlags, f) && !slices.Contains(coreFlags, f) {
			return nil, fmt.Errorf("unknown worker handler %q", f)
		}
	}
	var out []*amqp.Consumer
	for _, f := range handlerSet(flags).Consumers {
		h, err := newHandler(f, d)
		if err != nil {
			return nil, err
		}
		out = append(out, amqp.NewConsumer(h, d.Conn, d.Pub, d.Processed, d.Metrics, d.Log, d.Clock))
	}
	return out, nil
}

// handlerBuilders maps a feature flag to the constructor of its queue handler; a flag without an
// entry is refused at startup rather than leave its queue unconsumed.
var handlerBuilders = map[string]func(consumerDeps) (amqp.Handler, error){
	"evaluation": newEvaluationHandler,
	"memory":     newMemoryHandler,
	"learner":    newLearnerHandler,
	"summary":    newSummaryHandler,
}

func newHandler(name string, d consumerDeps) (amqp.Handler, error) {
	build, ok := handlerBuilders[name]
	if !ok {
		return nil, fmt.Errorf("unknown worker handler %q", name)
	}
	return build(d)
}

// errRouterRequired is a startup error: a consumer that calls providers cannot run without one.
var errRouterRequired = errors.New("worker handler needs the provider router")

// evaluationConfig maps config.Worker.Evaluation; platform/config does not import the feature
// packages, so the mapping lives here.
func evaluationConfig(cfg config.Worker) evaluation.Config {
	e := cfg.Evaluation
	return evaluation.Config{
		MinUserTurns: e.MinUserTurns, MinUserSpeechSeconds: e.MinUserSpeechSeconds, MinActiveSeconds: e.MinActiveSeconds,
		MinFindingConfidence: e.MinFindingConfidence, TranscriptMaxChars: e.TranscriptMaxChars, HandlerTimeout: e.HandlerTimeout,
	}
}

func summaryConfig(cfg config.Worker) summary.Config {
	return summary.Config{MaxChars: cfg.Summary.MaxChars}
}

// newEvaluationHandler wires the evaluation consumer by hand (D-004): Postgres store and source,
// the content repository, the evaluation provider of the router.
func newEvaluationHandler(d consumerDeps) (amqp.Handler, error) {
	if d.Router == nil {
		return nil, fmt.Errorf("%w: evaluation", errRouterRequired)
	}
	svc := evaluation.NewService(evaluation.Deps{
		Store: d.Store.EvaluationStore(), Source: postgres.NewEvaluationSource(d.Pool), Content: postgres.NewContentRepo(d.Pool),
		Router: d.Router, Cfg: d.EvalCfg, Clock: d.Clock, IDs: idgen.NewV7(),
		Metrics: evaluation.NewMetrics(d.Reg), Log: d.Log,
	})
	return evaluation.NewConsumer(svc, d.EvalCfg), nil
}

func newSummaryHandler(d consumerDeps) (amqp.Handler, error) {
	if d.Router == nil {
		return nil, fmt.Errorf("%w: summary", errRouterRequired)
	}
	svc := summary.NewService(summary.Deps{
		Store: d.Store.SummaryStore(), Source: postgres.NewSummarySource(d.Pool),
		Prompts: postgres.NewSummaryPromptSource(d.Pool), Router: d.Router, Cfg: d.SummaryCfg,
		Metrics: summary.NewMetrics(d.Reg), Log: d.Log,
	})
	return summary.NewConsumer(svc), nil
}

// routerMemoryProvider adapts the worker router to memory.Provider: the extraction call is one
// attempt on the active evaluation vendor (Evaluation(), never the retrying GenerateJSON: the repair
// call is the only second attempt, D-017); embeddings go through the router's embedding adapter.
type routerMemoryProvider struct{ r provider.AsyncRouter }

func (p routerMemoryProvider) GenerateJSON(ctx context.Context, req provider.StructuredRequest) (provider.StructuredResult, error) {
	return p.r.Evaluation().GenerateJSON(ctx, req)
}

func (p routerMemoryProvider) Embed(ctx context.Context, req provider.EmbeddingRequest) (provider.EmbeddingResult, error) {
	return p.r.Embed(ctx, req)
}

// newMemoryHandler wires the memory extraction consumer by hand (D-004): Postgres store, source,
// profile switch and prompt, the router as provider, the Redis memver bumper.
func newMemoryHandler(d consumerDeps) (amqp.Handler, error) {
	if d.Router == nil {
		return nil, fmt.Errorf("%w: memory", errRouterRequired)
	}
	ids := idgen.NewV7()
	mv := d.MemVer
	if mv == nil {
		mv = redisstore.NewMemVerBumper(d.Redis)
	}
	m := memory.NewMetrics(d.Reg)
	return memory.NewConsumer(memory.ConsumerDeps{
		Store: d.Store.MemoryStore(), Source: postgres.NewMemorySource(d.Pool, d.Clock),
		Settings: postgres.NewMemoryProfileSettings(d.Pool, ids, d.Clock), Prompts: postgres.NewMemoryPromptSource(d.Pool),
		Provider: routerMemoryProvider{r: d.Router}, MemVer: mv, Applier: memory.NewApplier(ids, d.Clock, m),
		Clock: d.Clock, IDs: ids, Log: d.Log, Metrics: m,
	}), nil
}

// newLearnerHandler wires the learner consumer: no provider, only Postgres.
func newLearnerHandler(d consumerDeps) (amqp.Handler, error) {
	return learner.NewConsumer(d.Store.LearnerStore(), postgres.NewLearnerUserGate(d.Pool), d.Clock, idgen.NewV7(), d.Log, learner.NewMetrics(d.Reg)), nil
}

// jobDeps are the collaborators of the scheduled jobs.
type jobDeps struct {
	Pool           *pgxpool.Pool
	Redis          redis.UniversalClient
	Clock          clock.Clock
	IDs            idgen.Generator
	Log            *slog.Logger
	Metrics        *jobs.Metrics
	SessionMetrics session.Metrics
	Cfg            config.Worker
}

// buildJobs wires all eight jobs of the table (spec 9.9.1) by hand (D-004); the scheduler runs only
// the ones the flags select, run-job may run any of them.
func buildJobs(d jobDeps) map[string]jobs.Job {
	pool, clk, log, m, cfg := d.Pool, d.Clock, d.Log, d.Metrics, d.Cfg
	store := postgres.NewStore(pool, postgres.WithIDs(d.IDs), postgres.WithClock(clk),
		postgres.WithTimeouts(cfg.LockTimeout, cfg.StatementTimeout))
	bal := balance.NewService(store.BalanceStore(), clk, d.IDs)
	sessions := postgres.NewSessionRepo(pool)
	// The worker closes sessions only as the sweeper (fence nil, producer WORKER) and abandons
	// RESERVED ones; it never creates or starts a session, so Resolver/Adaptive/Users stay unset.
	sessionSvc := session.NewService(session.Deps{
		Store: store.SessionStore(), Reader: sessions, Balance: bal,
		Control: redisstore.NewSessionControl(d.Redis), Clock: clk, IDs: d.IDs, Log: log,
		Metrics: d.SessionMetrics, ServiceName: "worker",
	})
	hb := redisstore.NewHeartbeatReader(d.Redis)
	retention := postgres.NewRetentionRepo(pool)

	return map[string]jobs.Job{
		"session_sweeper": jobs.NewSessionSweeperJob(func() jobs.SessionTicker {
			return session.NewSweeper(sessionSvc, sessions, hb, clk, log)
		}, m, log, clk, cfg.SweeperInterval),
		"reservation_cleanup": jobs.NewReservationCleanupJob(sessions, sessionSvc, m, clk, log),
		"transcript_retention": jobs.NewTranscriptRetentionJob(retention, jobs.RetentionConfig{
			TranscriptDays: cfg.RetentionTranscriptDays, SessionEventsDays: cfg.RetentionSessionEventsDays,
			ExpiredTokensDays: cfg.RetentionExpiredTokensDays,
		}, m, clk, log),
		"outbox_retention": jobs.NewOutboxRetentionJob(retention, jobs.OutboxRetentionConfig{
			PublishedDays: cfg.RetentionOutboxPublishedDays, ProcessedEventsDays: cfg.RetentionProcessedEventsDays,
		}, m, clk, log),
		"balance_reconcile": jobs.NewBalanceReconcileJob(jobs.BindReconciler(bal, postgres.NewReconcileRepo(pool)), m, log),
		"account_purge":     jobs.NewAccountPurgeJob(postgres.NewPurgeRepo(pool), cfg.AccountPurgeDelay, m, clk, log),
		"memory_cleanup":    jobs.NewMemoryCleanupJob(retention, redisstore.NewMemVerBumper(d.Redis), m, log),
		"partition_maintenance": jobs.NewPartitionMaintenanceJob(jobs.NewPgPartitionStore(postgres.NewPartitionRepo(pool)),
			jobs.PartitionConfig{RetentionTranscriptDays: cfg.RetentionTranscriptDays, RetentionSessionEventsDays: cfg.RetentionSessionEventsDays},
			m, clk, log),
	}
}

// openPool opens the pgxpool with the pool gauges on reg. closePool is safe to call twice.
func openPool(ctx context.Context, cfg config.Worker, reg *prometheus.Registry) (*pgxpool.Pool, func(), error) {
	var pool *pgxpool.Pool
	dbMetrics := metrics.NewDB(reg, func() metrics.PoolStat {
		if pool == nil {
			return metrics.PoolStat{}
		}
		return postgres.PoolStat(pool)
	})
	pool, err := postgres.NewPool(ctx, cfg.DB, postgres.PoolOptions{ServiceName: service, Metrics: dbMetrics})
	if err != nil {
		return nil, nil, err
	}
	return pool, pool.Close, nil
}

func openRedis(cfg config.Worker, reg *prometheus.Registry) (redis.UniversalClient, error) {
	return redisstore.NewClient(cfg.Redis, metrics.NewRedis(reg))
}

// dialRetry dials RabbitMQ until it works or ctx ends: a broker that comes up a few seconds after
// the worker must not crash-loop it (same stance as the startup checks of the ops listener).
func dialRetry(ctx context.Context, url, name string, log *slog.Logger) (*amqp.Conn, error) {
	for attempt := 0; ; attempt++ {
		conn, err := amqp.Dial(ctx, url, name, log)
		if err == nil {
			return conn, nil
		}
		if ctx.Err() != nil {
			return nil, err
		}
		delay := min(amqp.Backoff(attempt, rand.Float64), 5*time.Second)
		log.WarnContext(ctx, "rabbitmq not reachable yet", "conn", name, "retry_in", delay.String(), "error", err.Error())
		select {
		case <-ctx.Done():
			return nil, err
		case <-time.After(delay):
		}
	}
}

// keepAlive re-establishes conn whenever it drops, until ctx ends. The consumers reconnect their own
// connection inside Run; this is for the publisher connection, which nothing else watches (the
// outbox publisher only notices a dead broker when it has rows to send).
func keepAlive(ctx context.Context, conn *amqp.Conn, log *slog.Logger) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-conn.Closed():
		}
		if err := conn.Reconnect(ctx); err != nil {
			if ctx.Err() != nil {
				return
			}
			log.WarnContext(ctx, "amqp keep-alive reconnect failed", "error", err.Error())
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
			}
		}
	}
}

// liveConns lets /readyz see the AMQP connections that are dialed after the ops listener is up.
type liveConns struct{ v atomic.Pointer[[]*amqp.Conn] }

func (l *liveConns) set(c ...*amqp.Conn) { l.v.Store(&c) }

// check fails while any connection is down; with none dialed yet it fails too, so a worker that
// needs the broker is not ready before the dial.
func (l *liveConns) check(context.Context) error {
	p := l.v.Load()
	if p == nil {
		return errors.New("rabbitmq: not connected yet")
	}
	for _, c := range *p {
		if !c.Healthy() {
			return errors.New("rabbitmq: connection down")
		}
	}
	return nil
}

// goDone runs fn in a goroutine and returns a channel closed when it returned.
func goDone(fn func()) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	return done
}

func waitDone(ctx context.Context, done <-chan struct{}) error {
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// stopConsumers is spec 9.7.7 steps 3 to 5 for every consumer at once: each one stops taking
// deliveries and waits up to grace for its in-flight handlers (Consumer.Shutdown), then the run
// context is released and the Run goroutines are awaited. Handlers that outlive the grace see their
// root context cancelled and nack-requeue; that is not an error of the worker.
func stopConsumers(ctx context.Context, grace time.Duration, consumers []*amqp.Consumer, done []<-chan struct{}, stopRun func()) error {
	errs := make([]error, len(consumers))
	var wg sync.WaitGroup
	for i, c := range consumers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = c.Shutdown(ctx, grace)
		}()
	}
	wg.Wait()
	stopRun()
	for _, d := range done {
		errs = append(errs, waitDone(ctx, d))
	}
	return errors.Join(errs...)
}

// serve is server mode: it wires what the handler list selects, serves the ops listener and runs
// until ctx ends (signal) or a component fails, then shuts down in the order of spec 9.7.7.
func serve(ctx context.Context, handlersFlag string, stdout io.Writer) (retErr error) {
	cfg, err := config.LoadWorker()
	if err != nil {
		return err
	}
	handlers, err := config.ParseWorkerHandlers(handlersFlag, strings.Join(cfg.Handlers, ","))
	if err != nil {
		return err
	}
	logger := newLogger(cfg, stdout)
	slog.SetDefault(logger)
	logger.InfoContext(ctx, "starting", "build", buildinfo.String())
	logger.InfoContext(ctx, "worker handlers enabled", "handlers", strings.Join(handlers, ","))
	set := handlerSet(handlers)
	if len(set.Consumers) > 0 { // before any connection: a real configuration that cannot start fails here
		if err := validateProviders(); err != nil {
			logger.ErrorContext(ctx, "invalid provider configuration", "error", err)
			return err
		}
	}
	bg := context.WithoutCancel(ctx)

	otelShutdown, err := otel.Setup(ctx, otel.Config{
		Endpoint: cfg.Endpoint, ServiceName: cfg.ServiceName, ResourceAttributes: cfg.ResourceAttributes,
		Version: buildinfo.Version, Env: cfg.AppEnv,
	})
	if err != nil {
		return err
	}
	var otelOnce sync.Once
	flushOTel := func(c context.Context) (err error) {
		otelOnce.Do(func() { err = otelShutdown(c) })
		return err
	}
	defer func() { _ = flushOTel(bg) }()

	reg := metrics.NewRegistry()
	pool, closePool, err := openPool(ctx, cfg, reg)
	if err != nil {
		return err
	}
	defer closePool()
	rdb, err := openRedis(cfg, reg)
	if err != nil {
		return err
	}
	defer func() { _ = rdb.Close() }()

	conns := &liveConns{}
	checks := []health.Check{
		{Name: "postgres", Fn: pool.Ping},
		{Name: "redis", Fn: func(ctx context.Context) error { return redisstore.Ping(ctx, rdb) }},
	}
	live := []health.Check{{Name: "postgres", Fn: pool.Ping}}
	if set.NeedsAMQP() {
		checks = append(checks, health.Check{Name: "rabbitmq", Fn: conns.check})
		live = append(live, health.Check{Name: "rabbitmq", Fn: conns.check})
	}
	ops := health.NewOps(health.OpsOptions{
		Registry: reg, MetricsEnabled: cfg.MetricsEnabled, PprofEnabled: cfg.PprofEnabled, Logger: logger,
		Checks: checks, LiveChecks: live,
	})
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", cfg.HTTPAddr)
	if err != nil {
		return err
	}
	// The ops listener outlives the signal so /readyz can say "draining" during the shutdown; it
	// stops last.
	opsCtx, stopOps := context.WithCancel(bg)
	defer stopOps()
	serveErr := make(chan error, 1)
	go func() { serveErr <- ops.Serve(opsCtx, ln) }()

	checkCtx, cancelCheck := context.WithTimeout(ctx, 30*time.Second)
	defer cancelCheck()

	var am *amqp.Metrics
	var pubConn, consConn *amqp.Conn
	connCtx, stopConns := context.WithCancel(bg)
	defer stopConns()
	if set.NeedsAMQP() {
		am = amqp.NewMetrics(reg)
		url := cfg.Rabbit.URL.Reveal()
		if pubConn, err = dialRetry(checkCtx, url, service+"-publisher", logger); err != nil {
			return err
		}
		defer func() { _ = pubConn.Close() }()
		all := []*amqp.Conn{pubConn}
		if len(set.Consumers) > 0 {
			if consConn, err = dialRetry(checkCtx, url, service+"-consumer", logger); err != nil {
				return err
			}
			defer func() { _ = consConn.Close() }()
			all = append(all, consConn)
		}
		conns.set(all...)
	}

	var pubClient *amqp.ConfirmPublisher
	var retryPub amqp.Publisher
	if pubConn != nil {
		pubClient = amqp.NewConfirmPublisher(pubConn, cfg.ConfirmTimeout)
		retryPub = pubClient
	}
	// The router is built only for a worker that runs consumers, the only ones that call providers.
	var router provider.AsyncRouter
	if len(set.Consumers) > 0 {
		r, err := buildRouter(clock.System{}, logger, provider.NewMetrics(reg))
		if err != nil {
			return err
		}
		router = r
	}
	consumers, err := buildConsumers(handlers, consumerDeps{
		Conn: consConn, Pub: retryPub, Processed: postgres.NewProcessedEventsReader(pool),
		Metrics: am, Pool: pool, Redis: rdb,
		Store: postgres.NewStore(pool, postgres.WithClock(clock.System{}), postgres.WithTimeouts(cfg.LockTimeout, cfg.StatementTimeout)), Log: logger, Clock: clock.System{}, Reg: reg,
		Router: router, EvalCfg: evaluationConfig(cfg), SummaryCfg: summaryConfig(cfg),
	})
	if err != nil {
		return err
	}

	var sched *jobs.Scheduler
	if len(set.Jobs) > 0 {
		jm := jobs.NewMetrics(reg)
		js := buildJobs(jobDeps{
			Pool: pool, Redis: rdb, Clock: clock.System{}, IDs: idgen.NewV7(), Log: logger,
			Metrics: jm, SessionMetrics: metrics.NewSession(reg), Cfg: cfg,
		})
		sched = jobs.NewScheduler(jobs.SchedulerDeps{
			Locker: jobs.PgLocker{Pool: pool}, Metrics: jm, Clock: clock.System{}, Log: logger,
			SweeperInterval: cfg.SweeperInterval,
		})
		for _, spec := range jobs.SpecsForFlags(handlers) {
			sched.Register(spec, js[spec.Name])
		}
	}

	if err := ops.RunStartupChecks(checkCtx); err != nil {
		return err
	}

	// Everything below runs on bg: the signal only starts the shutdown sequence, which stops the
	// parts in order (spec 9.7.7) instead of cutting them all at once.
	fatal := make(chan error, len(consumers)+1)
	schedCtx, stopSched := context.WithCancel(bg)
	defer stopSched()
	var schedDone <-chan struct{}
	if sched != nil {
		schedDone = goDone(func() { sched.Run(schedCtx) })
	}

	pubCtx, stopPub := context.WithCancel(bg)
	defer stopPub()
	var pubDone []<-chan struct{}
	if set.Publisher {
		pub := amqp.NewOutboxPublisher(pool, pubClient, amqp.PublisherConfig{
			PollInterval: cfg.PollInterval, ConfirmTimeout: cfg.ConfirmTimeout, TxTimeout: cfg.TxTimeout,
			BatchSize: cfg.BatchSize, MaxAttempts: cfg.MaxAttempts,
		}, am, logger)
		pub.WaitReconnect = func(c context.Context) { _ = pubConn.Reconnect(c) }
		onErr := func(err error) { logger.WarnContext(bg, "gauge refresh failed", "error", err.Error()) }
		queues := queueNames()
		for _, q := range queueNames() {
			queues = append(queues, amqp.DLQName(q))
		}
		pubDone = append(pubDone,
			goDone(func() { _ = pub.Run(pubCtx) }),
			goDone(func() { amqp.RunOutboxGauges(pubCtx, pool, am, onErr) }),
			goDone(func() {
				amqp.RunQueueDepth(pubCtx, func() (*amqp091.Channel, error) { return pubConn.Channel() }, queues, am, onErr)
			}))
	}
	if pubConn != nil {
		go keepAlive(connCtx, pubConn, logger)
	}

	// Consumers run on a context only the end of the shutdown cancels; Consumer.Shutdown owns the
	// stop and the grace (cancelling the context alone would only stop taking deliveries).
	runCtx, stopRun := context.WithCancel(bg)
	defer stopRun()
	consDone := make([]<-chan struct{}, len(consumers))
	for i, c := range consumers {
		consDone[i] = goDone(func() {
			if err := c.Run(runCtx); err != nil && runCtx.Err() == nil {
				fatal <- fmt.Errorf("consumer stopped: %w", err)
			}
		})
	}

	lc := Lifecycle{
		SetNotReady: ops.SetDraining,
		StopScheduler: func(c context.Context) {
			stopSched()
			if schedDone != nil {
				if err := waitDone(c, schedDone); err != nil {
					logger.WarnContext(bg, "jobs still running at the end of the grace; their locks go with the connections")
				}
			}
		},
		ShutdownConsumers: func(c context.Context, grace time.Duration) error {
			return stopConsumers(c, grace, consumers, consDone, stopRun)
		},
		StopPublisher: func(c context.Context) error {
			stopPub()
			var errs []error
			for _, d := range pubDone {
				errs = append(errs, waitDone(c, d))
			}
			return errors.Join(errs...)
		},
		CloseAMQP: func() error {
			stopConns()
			var errs []error
			for _, c := range []*amqp.Conn{consConn, pubConn} {
				if c != nil {
					errs = append(errs, c.Close())
				}
			}
			return errors.Join(errs...)
		},
		ClosePool: func() {
			// pgxpool.Close waits for every acquired connection; a job that outran the grace must not
			// hold the exit hostage (its advisory lock dies with the process anyway).
			select {
			case <-goDone(closePool):
			case <-time.After(5 * time.Second):
				logger.WarnContext(bg, "pgx pool still has acquired connections, exiting without waiting")
			}
		},
		FlushOTel: func(context.Context) error {
			fctx, cancel := context.WithTimeout(bg, 5*time.Second)
			defer cancel()
			return flushOTel(fctx)
		},
	}
	logger.InfoContext(ctx, "ready", "ops_addr", ln.Addr().String(), "handlers", strings.Join(handlers, ","))

	var runErr error
	listenerDone := false
	select {
	case <-ctx.Done():
	case runErr = <-fatal:
		logger.ErrorContext(bg, "worker component failed, shutting down", "error", runErr.Error())
	case err := <-serveErr:
		listenerDone = true
		if err == nil {
			err = errors.New("ops listener stopped unexpectedly")
		}
		runErr = err
	}

	// A shutdown that runs out of time is logged, not fatal: whatever is left is redelivered
	// (requeue, idempotent handlers) or re-run (jobs) by the next pod.
	if err := lc.Shutdown(bg, cfg.ShutdownGrace); err != nil {
		logger.WarnContext(bg, "shutdown did not finish cleanly", "error", err.Error())
	}
	stopOps()
	if !listenerDone {
		if err := <-serveErr; err != nil && !errors.Is(err, context.Canceled) && runErr == nil {
			runErr = err
		}
	}
	logger.InfoContext(bg, "stopped")
	return runErr
}
