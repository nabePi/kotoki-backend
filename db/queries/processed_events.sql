-- name: MarkEventProcessed :execrows
INSERT INTO processed_events (consumer, event_id, event_type)
VALUES ($1, $2, $3)
ON CONFLICT (consumer, event_id) DO NOTHING;

-- name: IsEventProcessed :one
SELECT EXISTS (SELECT 1 FROM processed_events WHERE consumer = $1 AND event_id = $2);
