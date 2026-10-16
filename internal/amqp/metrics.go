package amqp

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"github.com/nabePi/kotoki-backend/internal/platform/metrics"
)

// Metrics are the Prometheus collectors of the messaging adapter (spec 9.7.9, 13.7.3). Labels are
// queue, event_type, result and reason only: never user_id or session_id.
type Metrics struct {
	ConsumerMessages   *prometheus.CounterVec   // queue, result
	ConsumerProcessing *prometheus.HistogramVec // queue
	ConsumerInflight   *prometheus.GaugeVec     // queue

	OutboxPublished        *prometheus.CounterVec // event_type
	OutboxFailures         *prometheus.CounterVec // event_type, reason = nack|timeout
	OutboxPending          prometheus.Gauge
	OutboxOldestPendingAge prometheus.Gauge
	OutboxFailed           prometheus.Gauge

	QueueMessages *prometheus.GaugeVec // queue
}

// NewMetrics registers the collectors on reg.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	f := promauto.With(reg)
	return &Metrics{
		ConsumerMessages: f.NewCounterVec(prometheus.CounterOpts{
			Name: "kotoki_consumer_messages_total", Help: "Consumer deliveries by outcome."}, []string{"queue", "result"}),
		ConsumerProcessing: f.NewHistogramVec(prometheus.HistogramOpts{
			Name: "kotoki_consumer_processing_seconds", Help: "Handler processing time per delivery.",
			Buckets: metrics.AsyncBuckets}, []string{"queue"}),
		ConsumerInflight: f.NewGaugeVec(prometheus.GaugeOpts{
			Name: "kotoki_consumer_inflight", Help: "Deliveries being processed."}, []string{"queue"}),
		OutboxPublished: f.NewCounterVec(prometheus.CounterOpts{
			Name: "kotoki_outbox_published_total", Help: "Outbox events confirmed by the broker."}, []string{"event_type"}),
		OutboxFailures: f.NewCounterVec(prometheus.CounterOpts{
			Name: "kotoki_outbox_publish_failures_total", Help: "Outbox publish attempts nacked or timed out."}, []string{"event_type", "reason"}),
		OutboxPending: f.NewGauge(prometheus.GaugeOpts{
			Name: "kotoki_outbox_pending", Help: "Outbox rows waiting to be published."}),
		OutboxOldestPendingAge: f.NewGauge(prometheus.GaugeOpts{
			Name: "kotoki_outbox_oldest_pending_age_seconds", Help: "Age of the oldest pending outbox row."}),
		OutboxFailed: f.NewGauge(prometheus.GaugeOpts{
			Name: "kotoki_outbox_failed", Help: "Outbox rows that failed permanently."}),
		QueueMessages: f.NewGaugeVec(prometheus.GaugeOpts{
			Name: "kotoki_rabbitmq_queue_messages", Help: "Messages ready in a RabbitMQ queue."}, []string{"queue"}),
	}
}
