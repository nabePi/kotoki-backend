package metrics

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// PoolStat is a snapshot of database connection pool usage.
type PoolStat struct{ Acquired, Idle, Max int32 }

// DB holds database client metrics.
type DB struct {
	query       *prometheus.HistogramVec
	acquireWait prometheus.Histogram
}

// NewDB registers database metrics on reg. stat is called on every scrape.
func NewDB(reg prometheus.Registerer, stat func() PoolStat) *DB {
	gauge := func(name, help string, f func(PoolStat) int32) prometheus.GaugeFunc {
		return prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: name, Help: help},
			func() float64 { return float64(f(stat())) })
	}
	d := &DB{
		query: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "kotoki_db_query_duration_seconds",
			Help:    "Database query duration in seconds.",
			Buckets: HTTPBuckets,
		}, []string{"query"}),
		acquireWait: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "kotoki_db_pool_acquire_wait_seconds",
			Help:    "Time spent waiting to acquire a pool connection.",
			Buckets: acquireWaitBuckets,
		}),
	}
	reg.MustRegister(
		gauge("kotoki_db_pool_acquired_conns", "Connections currently acquired.", func(s PoolStat) int32 { return s.Acquired }),
		gauge("kotoki_db_pool_idle_conns", "Idle connections in the pool.", func(s PoolStat) int32 { return s.Idle }),
		gauge("kotoki_db_pool_max_conns", "Maximum pool size.", func(s PoolStat) int32 { return s.Max }),
		d.query, d.acquireWait,
	)
	return d
}

// ObserveQuery records the duration of the named query.
func (d *DB) ObserveQuery(name string, dur time.Duration) {
	d.query.WithLabelValues(name).Observe(dur.Seconds())
}

// ObserveAcquireWait records time spent waiting for a pool connection.
func (d *DB) ObserveAcquireWait(dur time.Duration) {
	d.acquireWait.Observe(dur.Seconds())
}
