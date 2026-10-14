CREATE TABLE refresh_tokens (
    id             UUID PRIMARY KEY,
    user_id        UUID NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    family_id      UUID NOT NULL,              -- = id token pertama pada login; diwarisi saat rotasi
    parent_id      UUID NULL REFERENCES refresh_tokens(id),          -- NO ACTION: dicek di akhir statement
    token_hash     BYTEA NOT NULL,             -- SHA-256(token bytes), 32 byte
    user_agent     TEXT NULL,                  -- dipotong 512 karakter
    ip_address     INET NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at     TIMESTAMPTZ NOT NULL,
    revoked_at     TIMESTAMPTZ NULL,
    revoke_reason  TEXT NULL,
    replaced_by_id UUID NULL REFERENCES refresh_tokens(id),          -- NO ACTION

    CONSTRAINT chk_refresh_hash_len CHECK (octet_length(token_hash) = 32),
    CONSTRAINT chk_refresh_revoke CHECK ((revoked_at IS NULL) = (revoke_reason IS NULL)),
    CONSTRAINT chk_refresh_revoke_reason CHECK (revoke_reason IS NULL OR revoke_reason IN
        ('ROTATED', 'LOGOUT', 'REUSE_DETECTED', 'PASSWORD_RESET', 'USER_STATUS_CHANGED'))
);

CREATE UNIQUE INDEX ux_refresh_tokens_hash ON refresh_tokens (token_hash);
CREATE INDEX idx_refresh_tokens_family ON refresh_tokens (family_id);
CREATE INDEX idx_refresh_tokens_user_active ON refresh_tokens (user_id) WHERE revoked_at IS NULL;
CREATE INDEX idx_refresh_tokens_expires ON refresh_tokens (expires_at);

CREATE TABLE password_reset_tokens (
    id             UUID PRIMARY KEY,
    user_id        UUID NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    token_hash     BYTEA NOT NULL,             -- SHA-256, 32 byte
    requested_ip   INET NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at     TIMESTAMPTZ NOT NULL,       -- created_at + 15 menit
    used_at        TIMESTAMPTZ NULL,
    invalidated_at TIMESTAMPTZ NULL,           -- di-set saat token baru diminta / password berubah

    CONSTRAINT chk_reset_hash_len CHECK (octet_length(token_hash) = 32),
    CONSTRAINT chk_reset_expiry CHECK (expires_at > created_at)
);

CREATE UNIQUE INDEX ux_password_reset_tokens_hash ON password_reset_tokens (token_hash);
CREATE INDEX idx_password_reset_tokens_user_open
    ON password_reset_tokens (user_id) WHERE used_at IS NULL AND invalidated_at IS NULL;
