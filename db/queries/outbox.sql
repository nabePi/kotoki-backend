-- name: InsertOutboxEvent :exec
INSERT INTO event_outbox (event_id, event_type, event_version, aggregate_type, aggregate_id, user_id, producer,
                          payload, traceparent, tracestate, occurred_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11);
