package main

import "fmt"

// Expression helpers. Every expression is a plain string so the tests can scan it for metric names.

const (
	connRejectedFilter = `result!~"accepted|rejected_ratelimit|rejected_ticket|rejected_draining"`
	nonRetryDLQ        = `queue!~".*\\.(retry\\..*|dlq)"`
	retryDLQ           = `queue=~".*\\.(retry\\..*|dlq)"`
)

// quantile is histogram_quantile(q, sum by (le[, by]) (rate(metric_bucket{sel}[win]))).
func quantile(q float64, metric, sel, by, win string) string {
	g := "le"
	if by != "" {
		g += ", " + by
	}
	if sel != "" {
		sel = "{" + sel + "}"
	}
	return fmt.Sprintf("histogram_quantile(%g, sum by (%s) (rate(%s_bucket%s[%s])))", q, g, metric, sel, win)
}

// quantiles returns P50, P95 and P99 expressions of one histogram.
func quantiles(metric, sel, by string) []string {
	return []string{quantile(0.5, metric, sel, by, "5m"), quantile(0.95, metric, sel, by, "5m"), quantile(0.99, metric, sel, by, "5m")}
}

func ts(title, unit string, exprs ...string) Panel {
	return Panel{Title: title, Type: "timeseries", Unit: unit, Exprs: exprs}
}

func stat(title, unit string, exprs ...string) Panel {
	return Panel{Title: title, Type: "stat", Unit: unit, Exprs: exprs}
}

func heatmap(title, metric, sel string) Panel {
	if sel != "" {
		sel = "{" + sel + "}"
	}
	return Panel{Title: title, Type: "heatmap", Unit: "s",
		Exprs: []string{fmt.Sprintf("sum by (le) (increase(%s_bucket%s[$__rate_interval]))", metric, sel)}}
}

// All returns the six dashboards of spec §13.7.4 in file order.
func All() []Dashboard {
	return []Dashboard{overview(), realtimeLatency(), providers(), async(), dataStores(), businessIntegrity()}
}

func overview() Dashboard {
	const apiSvc = `service="kotoki-api"`
	return Dashboard{UID: "kotoki-overview", Title: "Kotoki Overview", Panels: []Panel{
		stat("Realtime capacity used", "percentunit",
			"sum(kotoki_realtime_sessions_active) / sum(kotoki_realtime_session_capacity)"),
		stat("Active sessions", "short", "sum(kotoki_realtime_sessions_active)"),
		stat("Firing alerts by severity", "short",
			`sum by (severity) (ALERTS{alertstate="firing",severity=~"critical|warning"}) or vector(0)`),
		stat("Draining realtime pods", "short", "sum(kotoki_realtime_draining)"),
		ts("Active sessions vs capacity", "short",
			"sum(kotoki_realtime_sessions_active)", "sum(kotoki_realtime_session_capacity)"),
		ts("Sessions created per second by result", "ops",
			"sum by (result) (rate(kotoki_sessions_created_total[5m]))"),
		ts("Sessions ended per second by end_reason", "ops",
			"sum by (end_reason) (rate(kotoki_sessions_ended_total[5m]))"),
		ts("API 5xx ratio", "percentunit",
			fmt.Sprintf(`sum(rate(kotoki_http_requests_total{%s,status=~"5.."}[5m])) / sum(rate(kotoki_http_requests_total{%s}[5m]))`, apiSvc, apiSvc)),
		ts("WebSocket connect failure ratio", "percentunit",
			fmt.Sprintf(`sum(rate(kotoki_realtime_connections_total{%s}[5m])) / sum(rate(kotoki_realtime_connections_total[5m]))`, connRejectedFilter)),
		ts("D-100 turn end to first audio P95 (target 1.5 s)", "s",
			quantile(0.95, "kotoki_realtime_turn_end_to_first_audio_seconds", `kind="reply"`, "", "5m")),
		ts("D-101 barge-in stop P95 (target 150 ms)", "s",
			quantile(0.95, "kotoki_realtime_bargein_stop_seconds", "", "", "5m")),
		ts("D-102 REST P95 (targets 200 ms, session create 300 ms)", "s",
			quantile(0.95, "kotoki_http_request_duration_seconds", apiSvc+`,route!~"/api/v1/realtime.*"`, "", "5m"),
			quantile(0.95, "kotoki_http_request_duration_seconds", apiSvc+`,route="/api/v1/sessions",method="POST"`, "", "5m")),
	}}
}

func realtimeLatency() Dashboard {
	const turn = "kotoki_realtime_turn_end_to_first_audio_seconds"
	return Dashboard{UID: "kotoki-realtime-latency", Title: "Realtime Latency", Panels: []Panel{
		heatmap("Turn end to first audio (heatmap, reply turns)", turn, `kind="reply"`),
		ts("Turn end to first audio P50/P95/P99 per kind", "s", quantiles(turn, "", "kind")...),
		ts("STT final P95", "s", quantile(0.95, "kotoki_realtime_stt_final_seconds", "", "provider", "5m")),
		ts("LLM first sentence P95", "s", quantile(0.95, "kotoki_realtime_llm_first_sentence_seconds", "", "provider, model", "5m")),
		ts("TTS first audio P95", "s", quantile(0.95, "kotoki_realtime_tts_first_audio_seconds", "", "provider, model", "5m")),
		ts("Server overhead P50/P95/P99 (target P95 150 ms)", "s", quantiles("kotoki_realtime_server_overhead_seconds", "", "")...),
		ts("Barge-in stop P50/P95/P99 (target P95 150 ms)", "s", quantiles("kotoki_realtime_bargein_stop_seconds", "", "")...),
		ts("Frame processing P95 per stage", "s", quantile(0.95, "kotoki_realtime_frame_processing_seconds", "", "stage", "5m")),
		ts("Outbound queue depth P95 per queue", "short", quantile(0.95, "kotoki_realtime_outbound_queue_depth", "", "queue", "5m")),
		ts("Slow consumers and dropped transient frames", "ops",
			"sum(rate(kotoki_realtime_slow_consumer_total[5m])) or vector(0)",
			"sum(rate(kotoki_realtime_outbound_transient_dropped_total[5m])) or vector(0)"),
		ts("Realtime CPU per pod (cores)", "short",
			`sum by (pod) (rate(container_cpu_usage_seconds_total{namespace="kotoki",pod=~"kotoki-realtime-.*",container="realtime"}[5m]))`),
		ts("Realtime memory per pod", "bytes",
			`sum by (pod) (container_memory_working_set_bytes{namespace="kotoki",pod=~"kotoki-realtime-.*",container="realtime"})`),
		ts("Sessions per pod by state", "short", "sum by (pod, state) (kotoki_realtime_sessions_active)"),
	}}
}

func providers() Dashboard {
	return Dashboard{UID: "kotoki-providers", Title: "Providers", Panels: []Panel{
		ts("Request rate per provider and capability", "reqps",
			"sum by (provider, capability) (rate(kotoki_provider_requests_total[5m]))"),
		ts("Error ratio per provider and capability", "percentunit",
			"(sum by (provider, capability) (rate(kotoki_provider_errors_total[5m])) or 0 * sum by (provider, capability) (rate(kotoki_provider_requests_total[5m]))) / sum by (provider, capability) (rate(kotoki_provider_requests_total[5m]))"),
		ts("Latency P95 per provider, capability and phase", "s",
			quantile(0.95, "kotoki_provider_latency_seconds", "", "provider, capability, phase", "5m")),
		ts("TTS fallback rate per from/to voice provider", "ops",
			`sum by (from, to) (rate(kotoki_provider_fallback_total{capability="TTS"}[5m])) or vector(0)`),
		ts("TTS fallback ratio (alert at 10%)", "percentunit",
			`(sum(rate(kotoki_provider_fallback_total{capability="TTS"}[15m])) or vector(0)) / sum(rate(kotoki_provider_requests_total{capability="TTS"}[15m]))`),
		ts("Circuit state per provider and capability (0 closed, 1 open, 2 half-open)", "short",
			"max by (provider, capability) (kotoki_provider_circuit_state)"),
		ts("Circuit transitions per second", "ops",
			"sum by (provider, capability, to) (rate(kotoki_provider_circuit_transitions_total[5m])) or vector(0)"),
		ts("Active streams per provider and capability", "short",
			"sum by (provider, capability) (kotoki_provider_active)"),
		ts("Local concurrency wait P95 and rejections", "s",
			quantile(0.95, "kotoki_provider_concurrency_wait_seconds", "", "provider, capability", "5m"),
			"sum by (provider, capability) (rate(kotoki_provider_concurrency_rejected_total[5m])) or vector(0)"),
		ts("STT connection rotations per second", "ops", "sum by (provider) (rate(kotoki_stt_rotations_total[5m]))"),
		ts("Usage per hour by unit (tokens, characters, audio seconds)", "short",
			"sum by (provider, capability, unit) (increase(kotoki_provider_usage_total[1h]))"),
		ts("Estimated cost per hour (USD)", "currencyUSD",
			"sum by (provider, capability) (increase(kotoki_provider_cost_usd_total[1h]))"),
		ts("Session cost P95 (USD) and cost per minute P95", "currencyUSD",
			quantile(0.95, "kotoki_session_cost_usd", "", "", "1h"),
			quantile(0.95, "kotoki_session_cost_per_minute_usd", "", "", "1h")),
	}}
}

func async() Dashboard {
	return Dashboard{UID: "kotoki-async", Title: "Async", Panels: []Panel{
		stat("Outbox pending", "short", "max(kotoki_outbox_pending)"),
		stat("Outbox oldest pending age", "s", "max(kotoki_outbox_oldest_pending_age_seconds)"),
		stat("Outbox failed", "short", "max(kotoki_outbox_failed)"),
		stat("DLQ messages", "short", `sum(kotoki_rabbitmq_queue_messages{queue=~".*\\.dlq"})`),
		ts("Outbox pending and oldest age", "short",
			"max(kotoki_outbox_pending)", "max(kotoki_outbox_oldest_pending_age_seconds)"),
		ts("Outbox published and failed per second", "ops",
			"sum(rate(kotoki_outbox_published_total[5m]))",
			"sum(rate(kotoki_outbox_publish_failures_total[5m])) or vector(0)"),
		ts("Queue depth per queue (main)", "short", "max by (queue) (kotoki_rabbitmq_queue_messages{"+nonRetryDLQ+"})"),
		ts("Queue depth per queue (retry and DLQ)", "short", "max by (queue) (kotoki_rabbitmq_queue_messages{"+retryDLQ+"})"),
		ts("Consumer processing time P95 per queue", "s", quantile(0.95, "kotoki_consumer_processing_seconds", "", "queue", "5m")),
		ts("Consumer results per queue", "ops", "sum by (queue, result) (rate(kotoki_consumer_messages_total[5m]))"),
		ts("Consumer in-flight deliveries", "short", "sum by (queue) (kotoki_consumer_inflight)"),
		ts("Evaluation latency P95", "s", quantile(0.95, "kotoki_evaluation_latency_seconds", "", "", "5m")),
		ts("Job runs per hour by result", "short", "sum by (job, result) (increase(kotoki_job_runs_total[1h]))"),
		ts("Job duration P95", "s", quantile(0.95, "kotoki_job_duration_seconds", "", "job", "1h")),
		ts("Seconds since last successful job run", "s", "time() - max by (job) (kotoki_job_last_success_timestamp_seconds)"),
	}}
}

func dataStores() Dashboard {
	return Dashboard{UID: "kotoki-data-stores", Title: "Data stores", Panels: []Panel{
		stat("Postgres up", "short", "min(pg_up)"),
		stat("Redis up", "short", "min(redis_up)"),
		stat("RabbitMQ nodes", "short", "count(rabbitmq_identity_info)"),
		stat("Postgres CPU", "percentunit", "max(kotoki:postgres_cpu_utilization:ratio)"),
		ts("Postgres CPU utilization", "percentunit", "max(kotoki:postgres_cpu_utilization:ratio)"),
		ts("Postgres connections by state", "short", "sum by (state) (pg_stat_activity_count)"),
		ts("App pool utilization per component (acquired / max)", "percentunit",
			"sum by (app_kubernetes_io_component) (kotoki_db_pool_acquired_conns) / sum by (app_kubernetes_io_component) (kotoki_db_pool_max_conns)"),
		ts("App pool acquire wait P95/P99", "s",
			quantile(0.95, "kotoki_db_pool_acquire_wait_seconds", "", "", "5m"),
			quantile(0.99, "kotoki_db_pool_acquire_wait_seconds", "", "", "5m")),
		ts("Top 10 queries by P95 duration", "s",
			"topk(10, "+quantile(0.95, "kotoki_db_query_duration_seconds", "", "query", "5m")+")"),
		ts("Redis commands per second", "ops", "sum(rate(redis_commands_processed_total[5m]))"),
		ts("Redis memory", "percentunit", "max(kotoki:redis_memory_utilization:ratio)"),
		ts("Redis memory used (bytes) and max", "bytes", "sum(redis_memory_used_bytes)", "sum(redis_memory_max_bytes)"),
		ts("Redis client errors per second", "ops",
			"sum(rate(kotoki_redis_command_errors_total[5m])) or vector(0)",
			"sum(rate(kotoki_redis_pool_timeouts_total[5m])) or vector(0)",
			"sum(rate(kotoki_ratelimit_backend_errors_total[5m])) or vector(0)"),
		ts("RabbitMQ unacked messages", "short", "sum(rabbitmq_queue_messages_unacked)"),
		ts("RabbitMQ memory (resident) per node", "bytes", "sum by (instance) (rabbitmq_process_resident_memory_bytes)"),
		ts("RabbitMQ free disk per node", "bytes", "sum by (instance) (rabbitmq_disk_space_available_bytes)"),
		ts("RabbitMQ alarms (memory, disk)", "short",
			"max(rabbitmq_alarms_memory_used_watermark)", "max(rabbitmq_alarms_free_disk_space_watermark)"),
	}}
}

func businessIntegrity() Dashboard {
	return Dashboard{UID: "kotoki-business", Title: "Business integrity", Panels: []Panel{
		stat("Balance invariant violations", "short", "sum(kotoki_balance_invariant_violations)"),
		stat("FAILED sessions, last hour", "short", `sum(increase(kotoki_sessions_ended_total{status="FAILED"}[1h])) or vector(0)`),
		ts("Balance invariant violations per check", "short", "max by (check) (kotoki_balance_invariant_violations)"),
		ts("Settlement: sessions ended per hour by end_reason and status", "short",
			"sum by (end_reason, status) (increase(kotoki_sessions_ended_total[1h]))"),
		ts("FAILED sessions per hour", "short", `sum by (end_reason) (increase(kotoki_sessions_ended_total{status="FAILED"}[1h])) or vector(0)`),
		ts("Balance reconcile items per hour", "short",
			`sum by (action) (increase(kotoki_job_items_total{job="balance_reconcile"}[1h])) or vector(0)`),
		ts("Seconds since last balance reconcile and reservation cleanup", "s",
			`time() - max(kotoki_job_last_success_timestamp_seconds{job="balance_reconcile"})`,
			`time() - max(kotoki_job_last_success_timestamp_seconds{job="reservation_cleanup"})`),
		ts("Session sweeper actions per hour", "short", "sum by (rule, result) (increase(kotoki_sweeper_actions_total[1h]))"),
		ts("Partition months ahead per table", "short", "min by (table) (kotoki_partition_months_ahead)"),
	}}
}
