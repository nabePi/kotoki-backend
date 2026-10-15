-- migrations/000017_messaging.up.sql
CREATE TABLE event_outbox (
    event_id        UUID        PRIMARY KEY,               -- UUIDv7 dibuat aplikasi; = envelope.event_id
    event_type      TEXT        NOT NULL,                  -- = routing key
    event_version   INTEGER     NOT NULL DEFAULT 1,
    aggregate_type  TEXT        NOT NULL,
    aggregate_id    UUID        NOT NULL,
    user_id         UUID        NOT NULL,                  -- tanpa FK: outbox adalah infrastruktur
    producer        TEXT        NOT NULL,
    payload         JSONB       NOT NULL,
    traceparent     TEXT        NULL,                      -- W3C trace context saat Enqueue (R-31); NULL jika tidak ada span aktif
    tracestate      TEXT        NULL,
    occurred_at     TIMESTAMPTZ NOT NULL,                  -- waktu kejadian domain (UTC)
    status          TEXT        NOT NULL DEFAULT 'PENDING',
    attempts        INTEGER     NOT NULL DEFAULT 0,        -- jumlah publish yang gagal (nack/timeout)
    available_at    TIMESTAMPTZ NOT NULL DEFAULT now(),    -- baris baru boleh dipublish setelah waktu ini
    last_error      TEXT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    published_at    TIMESTAMPTZ,

    CONSTRAINT chk_event_outbox_event_type CHECK (event_type IN (
        'session.completed', 'evaluation.completed', 'evaluation.failed', 'memory.extracted')),
    CONSTRAINT chk_event_outbox_version CHECK (event_version >= 1),
    CONSTRAINT chk_event_outbox_aggregate_type CHECK (aggregate_type IN ('SESSION', 'EVALUATION')),
    CONSTRAINT chk_event_outbox_producer CHECK (producer IN ('API', 'REALTIME', 'WORKER')),
    CONSTRAINT chk_event_outbox_status CHECK (status IN ('PENDING', 'PUBLISHED', 'FAILED')),
    CONSTRAINT chk_event_outbox_attempts CHECK (attempts >= 0),
    CONSTRAINT chk_event_outbox_published_at CHECK ((status = 'PUBLISHED') = (published_at IS NOT NULL)),
    CONSTRAINT chk_event_outbox_payload_size CHECK (octet_length(payload::text) <= 65536),
    CONSTRAINT chk_event_outbox_last_error_len CHECK (last_error IS NULL OR length(last_error) <= 2000)
);

-- Antrian siap publish (partial, kecil karena baris PUBLISHED keluar dari index).
CREATE INDEX idx_event_outbox_ready
    ON event_outbox (available_at, event_id)
    WHERE status = 'PENDING';

-- Retensi.
CREATE INDEX idx_event_outbox_published_at
    ON event_outbox (published_at)
    WHERE status = 'PUBLISHED';

-- Monitoring baris gagal permanen.
CREATE INDEX idx_event_outbox_failed
    ON event_outbox (created_at)
    WHERE status = 'FAILED';

-- Debug / audit per aggregate.
CREATE INDEX idx_event_outbox_aggregate
    ON event_outbox (aggregate_type, aggregate_id);

CREATE TABLE processed_events (
    consumer      TEXT        NOT NULL,    -- = nama queue consumer
    event_id      UUID        NOT NULL,
    event_type    TEXT        NOT NULL,
    processed_at  TIMESTAMPTZ NOT NULL DEFAULT now(),

    PRIMARY KEY (consumer, event_id),
    CONSTRAINT chk_processed_events_consumer CHECK (consumer IN (
        'evaluation.session_completed',
        'memory.session_completed',
        'summary.session_completed',
        'learner.evaluation_completed'))
);

CREATE INDEX idx_processed_events_processed_at ON processed_events (processed_at);
