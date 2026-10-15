package metrics

import "github.com/prometheus/client_golang/prometheus"

// Redis holds Redis client metrics.
type Redis struct {
	cmdErrors    *prometheus.CounterVec
	poolTimeouts prometheus.Counter
}

// NewRedis registers Redis metrics on reg.
func NewRedis(reg prometheus.Registerer) *Redis {
	r := &Redis{
		cmdErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kotoki_redis_command_errors_total",
			Help: "Redis command errors by operation.",
		}, []string{"op"}),
		poolTimeouts: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "kotoki_redis_pool_timeouts_total",
			Help: "Redis pool connection timeouts.",
		}),
	}
	reg.MustRegister(r.cmdErrors, r.poolTimeouts)
	return r
}

// CommandError counts a failed Redis command for op.
func (r *Redis) CommandError(op string) { r.cmdErrors.WithLabelValues(op).Inc() }

// PoolTimeout counts a Redis pool timeout.
func (r *Redis) PoolTimeout() { r.poolTimeouts.Inc() }
