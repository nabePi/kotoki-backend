package jobs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"github.com/nabePi/kotoki-backend/internal/platform/clock"
)

const (
	partitionMaintenanceName = "partition_maintenance"
	tableSessionTurns        = "session_turns"
	tableSessionEvents       = "session_events"

	// partitionsAhead is how many months after the current one are kept ready (spec 9.9.10).
	partitionsAhead = 3
	// turnsRetentionMargin is the 2 hour margin of transcript_retention step 2 (spec 9.9.4): a turn
	// is created at most ~1 h 5 min before the session's ended_at, so a partition whose upper bound
	// is older than now-180d-2h holds nothing that is younger than 180 days after ended_at.
	turnsRetentionMargin = 2 * time.Hour
)

// partitionedTables are the only tables this job may run DDL on, in processing order.
var partitionedTables = []string{tableSessionTurns, tableSessionEvents}

// Partition is one monthly range partition (bounds are UTC, Lower inclusive, Upper exclusive).
type Partition struct {
	Name          string
	Lower, Upper  time.Time
	DetachPending bool // an earlier DETACH ... CONCURRENTLY was interrupted: needs FINALIZE
}

// PartitionStore reads and changes the partitions of one partitioned table.
type PartitionStore interface {
	List(ctx context.Context, table string) ([]Partition, error)
	Ensure(ctx context.Context, table string, month time.Time) error
	Drop(ctx context.Context, table string, p Partition) error
}

// PartitionConfig carries the retention windows that decide which partitions are dropped.
type PartitionConfig struct {
	RetentionTranscriptDays, RetentionSessionEventsDays int
}

// monthStart is the first instant of t's month in UTC.
func monthStart(t time.Time) time.Time {
	u := t.UTC()
	return time.Date(u.Year(), u.Month(), 1, 0, 0, 0, 0, time.UTC)
}

// PartitionName is `{table}_pYYYYMM` for the UTC month containing month.
func PartitionName(table string, month time.Time) string {
	return table + "_p" + monthStart(month).Format("200601")
}

// PlanCreate lists the months (UTC month starts) that must exist: the current one and the next 3.
func PlanCreate(now time.Time) []time.Time {
	first := monthStart(now)
	out := make([]time.Time, 0, partitionsAhead+1)
	for i := 0; i <= partitionsAhead; i++ {
		out = append(out, first.AddDate(0, i, 0))
	}
	return out
}

var boundRE = regexp.MustCompile(`^FOR VALUES FROM \('([^']+)'\) TO \('([^']+)'\)$`)

// boundLayouts cover how timestamptz literals appear in pg_get_expr, whatever the session time zone.
var boundLayouts = []string{
	"2006-01-02 15:04:05.999999-07:00:00",
	"2006-01-02 15:04:05.999999-07:00",
	"2006-01-02 15:04:05.999999-07",
}

func parseBoundTime(s string) (time.Time, error) {
	for _, l := range boundLayouts {
		if t, err := time.Parse(l, s); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("jobs: unparsable partition bound %q", s)
}

// ParseBound reads the `pg_get_expr(relpartbound)` text of a RANGE partition. DEFAULT, MINVALUE
// and MAXVALUE bounds are errors: this job only manages bounded monthly partitions.
func ParseBound(expr string) (lower, upper time.Time, err error) {
	m := boundRE.FindStringSubmatch(strings.TrimSpace(expr))
	if m == nil {
		return time.Time{}, time.Time{}, fmt.Errorf("jobs: unsupported partition bound %q", expr)
	}
	if lower, err = parseBoundTime(m[1]); err != nil {
		return time.Time{}, time.Time{}, err
	}
	if upper, err = parseBoundTime(m[2]); err != nil {
		return time.Time{}, time.Time{}, err
	}
	return lower, upper, nil
}

// ExpiredPartitions picks the partitions that may be dropped. session_turns: upper <
// now-retention-2h; session_events: upper < now-retention; any other table: none. A partition
// whose upper bound is on or after the start of the current UTC month is never returned,
// whatever the retention (a code guard, spec 9.9.10).
func ExpiredPartitions(table string, parts []Partition, now time.Time, retentionDays int) []Partition {
	var cutoff time.Time
	switch table {
	case tableSessionTurns:
		cutoff = now.Add(-time.Duration(retentionDays)*24*time.Hour - turnsRetentionMargin)
	case tableSessionEvents:
		cutoff = now.Add(-time.Duration(retentionDays) * 24 * time.Hour)
	default:
		return nil
	}
	current := monthStart(now)
	var out []Partition
	for _, p := range parts {
		if !p.Upper.Before(current) {
			continue
		}
		if p.Upper.Before(cutoff) {
			out = append(out, p)
		}
	}
	return out
}

// PartitionMaintenanceJob keeps monthly partitions of session_turns and session_events ahead of
// time and drops the ones that are entirely past retention (spec 9.9.10). It only does partition
// DDL; deleting rows inside the boundary partition is transcript_retention's job.
type PartitionMaintenanceJob struct {
	s   PartitionStore
	cfg PartitionConfig
	m   *Metrics
	clk clock.Clock
	log *slog.Logger
}

// NewPartitionMaintenanceJob builds the job.
func NewPartitionMaintenanceJob(s PartitionStore, cfg PartitionConfig, m *Metrics, clk clock.Clock, log *slog.Logger) *PartitionMaintenanceJob {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &PartitionMaintenanceJob{s: s, cfg: cfg, m: m, clk: clk, log: log}
}

// Run implements Job. A failing partition is logged at ERROR and the job moves on (one failed run
// does not matter thanks to the 3 month buffer); all failures are returned together at the end so
// the run is still counted as failed.
func (j *PartitionMaintenanceJob) Run(ctx context.Context) error {
	now := j.clk.Now()
	var errs []error
	for _, table := range partitionedTables {
		if err := ctx.Err(); err != nil {
			return errors.Join(append(errs, err)...)
		}
		errs = append(errs, j.maintain(ctx, table, now)...)
	}
	return errors.Join(errs...)
}

func (j *PartitionMaintenanceJob) fail(ctx context.Context, msg, table, name string, err error) error {
	j.log.ErrorContext(ctx, msg, "table", table, "partition", name, "error", err.Error())
	j.m.JobItems.WithLabelValues(partitionMaintenanceName, "error").Inc()
	return fmt.Errorf("%s %s %s: %w", msg, table, name, err)
}

func (j *PartitionMaintenanceJob) maintain(ctx context.Context, table string, now time.Time) []error {
	var errs []error
	existing, listErr := j.s.List(ctx, table)
	if listErr != nil {
		errs = append(errs, j.fail(ctx, "partition_maintenance: list failed", table, "", listErr))
	}
	have := make(map[string]bool, len(existing))
	for _, p := range existing {
		have[p.Name] = true
	}

	// 1) create ahead. Without a listing every month is attempted (IF NOT EXISTS makes that safe)
	// but nothing is counted as created.
	for _, month := range PlanCreate(now) {
		name := PartitionName(table, month)
		if have[name] {
			continue
		}
		if err := j.s.Ensure(ctx, table, month); err != nil {
			errs = append(errs, j.fail(ctx, "partition_maintenance: create failed", table, name, err))
			continue
		}
		if listErr == nil {
			j.m.PartitionsCreated.WithLabelValues(table).Inc()
			j.m.JobItems.WithLabelValues(partitionMaintenanceName, "created").Inc()
		}
	}
	if listErr != nil {
		return errs
	}

	// 2) drop expired, then 3) report the buffer from a fresh listing.
	retention := j.cfg.RetentionTranscriptDays
	if table == tableSessionEvents {
		retention = j.cfg.RetentionSessionEventsDays
	}
	for _, p := range ExpiredPartitions(table, existing, now, retention) {
		if err := j.s.Drop(ctx, table, p); err != nil {
			errs = append(errs, j.fail(ctx, "partition_maintenance: drop failed", table, p.Name, err))
			continue
		}
		j.m.PartitionsDropped.WithLabelValues(table).Inc()
		j.m.JobItems.WithLabelValues(partitionMaintenanceName, "dropped").Inc()
		j.log.InfoContext(ctx, "partition_maintenance: dropped", "table", table, "partition", p.Name)
	}
	after, err := j.s.List(ctx, table)
	if err != nil {
		return append(errs, j.fail(ctx, "partition_maintenance: relist failed", table, "", err))
	}
	ahead := 0
	for _, p := range after {
		if p.Lower.After(now) {
			ahead++
		}
	}
	j.m.PartitionMonthsAhead.WithLabelValues(table).Set(float64(ahead))
	return errs
}
