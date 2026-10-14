CREATE TABLE balance_accounts (
    id                UUID PRIMARY KEY,
    user_id           UUID NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    available_seconds BIGINT NOT NULL DEFAULT 0,
    reserved_seconds  BIGINT NOT NULL DEFAULT 0,
    version           BIGINT NOT NULL DEFAULT 0,       -- naik setiap ApplyMovement
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT ux_balance_accounts_user UNIQUE (user_id),
    CONSTRAINT chk_balance_available_non_negative CHECK (available_seconds >= 0),
    CONSTRAINT chk_balance_reserved_non_negative  CHECK (reserved_seconds >= 0)
);
