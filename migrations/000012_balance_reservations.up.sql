CREATE TABLE balance_reservations (
    id                UUID PRIMARY KEY,
    user_id           UUID NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    account_id        UUID NOT NULL REFERENCES balance_accounts(id) ON DELETE RESTRICT,
    session_id        UUID NOT NULL REFERENCES sessions(id) ON DELETE RESTRICT,
    reserved_seconds  BIGINT NOT NULL,
    consumed_seconds  BIGINT NULL,          -- diisi saat SETTLED/RELEASED
    released_seconds  BIGINT NULL,          -- diisi saat SETTLED/RELEASED
    status            TEXT NOT NULL DEFAULT 'HELD',
    start_deadline_at TIMESTAMPTZ NOT NULL, -- created_at + 5 menit (D-035)
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    settled_at        TIMESTAMPTZ NULL,
    released_at       TIMESTAMPTZ NULL,
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT ux_balance_reservations_session UNIQUE (session_id),
    CONSTRAINT chk_reservation_status CHECK (status IN ('HELD', 'SETTLED', 'RELEASED')),
    CONSTRAINT chk_reservation_reserved CHECK (reserved_seconds IN (300, 600, 900, 1800, 2700, 3600)),
    CONSTRAINT chk_reservation_deadline CHECK (start_deadline_at > created_at),
    CONSTRAINT chk_reservation_state CHECK (
        (status = 'HELD'
            AND consumed_seconds IS NULL AND released_seconds IS NULL
            AND settled_at IS NULL AND released_at IS NULL)
     OR (status = 'SETTLED'
            AND consumed_seconds BETWEEN 0 AND reserved_seconds
            AND released_seconds = reserved_seconds - consumed_seconds
            AND settled_at IS NOT NULL AND released_at IS NULL)
     OR (status = 'RELEASED'
            AND consumed_seconds = 0 AND released_seconds = reserved_seconds
            AND released_at IS NOT NULL AND settled_at IS NULL)
    )
);

CREATE INDEX idx_balance_reservations_held_deadline
    ON balance_reservations (start_deadline_at) WHERE status = 'HELD';
CREATE INDEX idx_balance_reservations_user ON balance_reservations (user_id, created_at DESC);
