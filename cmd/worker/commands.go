package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"slices"

	"github.com/nabePi/kotoki-backend/internal/amqp"
	"github.com/nabePi/kotoki-backend/internal/jobs"
	"github.com/nabePi/kotoki-backend/internal/platform/buildinfo"
	"github.com/nabePi/kotoki-backend/internal/platform/clock"
	"github.com/nabePi/kotoki-backend/internal/platform/config"
	"github.com/nabePi/kotoki-backend/internal/platform/idgen"
	"github.com/nabePi/kotoki-backend/internal/platform/logging"
	"github.com/nabePi/kotoki-backend/internal/platform/metrics"
)

// newLogger builds the worker logger on w (stderr for subcommands, so stdout stays the result).
func newLogger(cfg config.Worker, w io.Writer) *slog.Logger {
	var level slog.Level
	if err := level.UnmarshalText([]byte(cfg.LogLevel)); err != nil {
		level = slog.LevelInfo
	}
	return logging.New(w, logging.Options{
		Level: level, Format: cfg.LogFormat, Service: service,
		Version: buildinfo.Version, GitCommit: buildinfo.GitCommit, Env: cfg.AppEnv, LogTranscripts: cfg.LogTranscripts,
	})
}

// runJobCmd is `worker run-job <name>`: one run of the named job under its advisory lock, spec
// timeout and metrics, whatever --handlers says. It prints `job=<name> ran=<true|false>` and exits 0
// also when another instance holds the lock (ran=false); a failing job is exit 1, an unknown name 2.
func runJobCmd(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("run-job", flag.ContinueOnError)
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		return usageError{err.Error()}
	}
	if fs.NArg() != 1 {
		return usagef("usage: worker run-job <name> (one of %v)", jobNames())
	}
	name := fs.Arg(0)
	spec, ok := jobs.SpecByName(name)
	if !ok {
		return usagef("run-job: unknown job %q (one of %v)", name, jobNames())
	}

	cfg, err := config.LoadWorker()
	if err != nil {
		return err
	}
	logger := newLogger(cfg, stderr)
	slog.SetDefault(logger)

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

	jm := jobs.NewMetrics(reg)
	js := buildJobs(jobDeps{
		Pool: pool, Redis: rdb, Clock: clock.System{}, IDs: idgen.NewV7(), Log: logger,
		Metrics: jm, SessionMetrics: metrics.NewSession(reg), Cfg: cfg,
	})
	sched := jobs.NewScheduler(jobs.SchedulerDeps{
		Locker: jobs.PgLocker{Pool: pool}, Metrics: jm, Clock: clock.System{}, Log: logger,
		SweeperInterval: cfg.SweeperInterval,
	})
	sched.Register(spec, js[name])

	ran, err := sched.RunOnce(ctx, name)
	if err != nil {
		return fmt.Errorf("run-job %s: %w", name, err)
	}
	_, _ = fmt.Fprintf(stdout, "job=%s ran=%t\n", name, ran)
	return nil
}

func jobNames() []string {
	out := make([]string, len(jobs.Specs))
	for i, s := range jobs.Specs {
		out[i] = s.Name
	}
	return out
}

// dlqReplayCmd is `worker dlq-replay --queue <Q> [--limit 100] [--dry-run]` (spec 9.7.8): messages
// of <Q>.dlq go back to <Q> with the retry count reset; --dry-run only lists them. It prints the
// number of messages moved (or listed).
func dlqReplayCmd(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("dlq-replay", flag.ContinueOnError)
	fs.SetOutput(stderr)
	queue := fs.String("queue", "", "main queue whose DLQ is replayed")
	limit := fs.Int("limit", 100, "maximum number of messages (0 = the whole DLQ)")
	dry := fs.Bool("dry-run", false, "list the messages without moving them")
	if err := fs.Parse(args); err != nil {
		return usageError{err.Error()}
	}
	if fs.NArg() > 0 {
		return usagef("dlq-replay: unexpected argument %q", fs.Arg(0))
	}
	if !slices.ContainsFunc(amqp.Queues, func(q amqp.QueueSpec) bool { return q.Name == *queue }) {
		return usagef("dlq-replay: --queue must be one of %v, got %q", queueNames(), *queue)
	}
	if *limit < 0 {
		return usagef("dlq-replay: --limit must be >= 0, got %d", *limit)
	}

	cfg, err := config.LoadWorker()
	if err != nil {
		return err
	}
	logger := newLogger(cfg, stderr)
	slog.SetDefault(logger)

	conn, err := amqp.Dial(ctx, cfg.Rabbit.URL.Reveal(), service+"-dlq-replay", logger)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	pub := amqp.NewConfirmPublisher(conn, cfg.ConfirmTimeout)

	n, err := amqp.ReplayDLQ(ctx, conn, pub, amqp.ReplayOptions{Queue: *queue, Limit: *limit, DryRun: *dry, Out: stdout})
	if err != nil {
		return fmt.Errorf("dlq-replay %s: %w (moved %d before the error)", *queue, err, n)
	}
	if *dry {
		_, _ = fmt.Fprintf(stdout, "queue=%s dry_run=true listed=%d\n", *queue, n)
	} else {
		_, _ = fmt.Fprintf(stdout, "queue=%s replayed=%d\n", *queue, n)
	}
	return nil
}

func queueNames() []string {
	out := make([]string, len(amqp.Queues))
	for i, q := range amqp.Queues {
		out[i] = q.Name
	}
	return out
}
