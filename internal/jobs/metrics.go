package jobs

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"github.com/nabePi/kotoki-backend/internal/platform/metrics"
)

// Metrics are the Prometheus collectors of the scheduled jobs (spec 9.9, 13.7.3). Labels are job,
// result, action, rule, check and table only: never user_id or session_id.
type Metrics struct {
	JobRuns        *prometheus.CounterVec   // job, result = ok|error|skipped_locked
	JobDuration    *prometheus.HistogramVec // job
	JobItems       *prometheus.CounterVec   // job, action
	JobLastSuccess *prometheus.GaugeVec     // job, epoch seconds

	MemoryCleanupDeleted prometheus.Counter // kotoki_memory_cleanup_deleted_total (spec 13.7, 11.8.3)

	SweeperActions           *prometheus.CounterVec // rule = a..e, result = ok|noop|error
	HeartbeatChecksSuspended prometheus.Gauge
	MassHeartbeatLoss        prometheus.Counter

	BalanceInvariantViolations *prometheus.GaugeVec // check
	PartitionMonthsAhead       *prometheus.GaugeVec // table
	PartitionsCreated          *prometheus.CounterVec
	PartitionsDropped          *prometheus.CounterVec
}

// NewMetrics registers the collectors on reg.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	f := promauto.With(reg)
	return &Metrics{
		JobRuns: f.NewCounterVec(prometheus.CounterOpts{
			Name: "kotoki_job_runs_total", Help: "Scheduled job runs by result."}, []string{"job", "result"}),
		JobDuration: f.NewHistogramVec(prometheus.HistogramOpts{
			Name: "kotoki_job_duration_seconds", Help: "Duration of one scheduled job run.",
			Buckets: metrics.AsyncBuckets}, []string{"job"}),
		JobItems: f.NewCounterVec(prometheus.CounterOpts{
			Name: "kotoki_job_items_total", Help: "Items a job processed, by action."}, []string{"job", "action"}),
		JobLastSuccess: f.NewGaugeVec(prometheus.GaugeOpts{
			Name: "kotoki_job_last_success_timestamp_seconds", Help: "Unix time of the last successful run."}, []string{"job"}),
		MemoryCleanupDeleted: f.NewCounter(prometheus.CounterOpts{
			Name: "kotoki_memory_cleanup_deleted_total", Help: "Expired memories hard-deleted by memory_cleanup."}),
		SweeperActions: f.NewCounterVec(prometheus.CounterOpts{
			Name: "kotoki_sweeper_actions_total", Help: "Session sweeper actions by rule and result."}, []string{"rule", "result"}),
		HeartbeatChecksSuspended: f.NewGauge(prometheus.GaugeOpts{
			Name: "kotoki_sweeper_heartbeat_checks_suspended", Help: "1 while the heartbeat rule is suspended."}),
		MassHeartbeatLoss: f.NewCounter(prometheus.CounterOpts{
			Name: "kotoki_sweeper_mass_heartbeat_loss_total", Help: "Ticks where the heartbeat rule was skipped for mass loss."}),
		BalanceInvariantViolations: f.NewGaugeVec(prometheus.GaugeOpts{
			Name: "kotoki_balance_invariant_violations", Help: "Rows violating a balance invariant, by check."}, []string{"check"}),
		PartitionMonthsAhead: f.NewGaugeVec(prometheus.GaugeOpts{
			Name: "kotoki_partition_months_ahead", Help: "Months of partitions ahead of the current month."}, []string{"table"}),
		PartitionsCreated: f.NewCounterVec(prometheus.CounterOpts{
			Name: "kotoki_partitions_created_total", Help: "Partitions created."}, []string{"table"}),
		PartitionsDropped: f.NewCounterVec(prometheus.CounterOpts{
			Name: "kotoki_partitions_dropped_total", Help: "Partitions dropped."}, []string{"table"}),
	}
}
