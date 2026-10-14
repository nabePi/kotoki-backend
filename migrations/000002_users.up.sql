CREATE TABLE users (
    id                  UUID PRIMARY KEY,                 -- UUIDv7 dari aplikasi
    email               CITEXT NOT NULL,
    phone_number        TEXT NULL,                        -- E.164, mis. +628123456789
    full_name           TEXT NOT NULL,
    password_hash       TEXT NOT NULL,                    -- PHC argon2id
    role                TEXT NOT NULL DEFAULT 'USER',
    status              TEXT NOT NULL DEFAULT 'ACTIVE',
    password_changed_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_login_at       TIMESTAMPTZ NULL,

    terms_version            TEXT NOT NULL,                   -- versi terms+privacy yang disetujui (= PRIVACY_POLICY_VERSION saat itu)
    terms_accepted_at        TIMESTAMPTZ NOT NULL,

    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    deleted_at          TIMESTAMPTZ NULL,                     -- saat status menjadi DELETED (DELETE /me atau admin)
    purged_at           TIMESTAMPTZ NULL,                     -- saat job account_purge selesai (Bab 13 §13.9.3)

    CONSTRAINT chk_users_role   CHECK (role IN ('USER', 'ADMIN')),
    CONSTRAINT chk_users_status CHECK (status IN ('ACTIVE', 'SUSPENDED', 'DELETED')),
    CONSTRAINT chk_users_deleted_at CHECK ((status = 'DELETED') = (deleted_at IS NOT NULL)),
    CONSTRAINT chk_users_purged_at CHECK (purged_at IS NULL OR (status = 'DELETED' AND purged_at >= deleted_at)),
    CONSTRAINT chk_users_email_len CHECK (char_length(email) BETWEEN 3 AND 254),
    CONSTRAINT chk_users_phone_fmt CHECK (phone_number IS NULL OR phone_number ~ '^\+[1-9][0-9]{7,14}$'),
    CONSTRAINT chk_users_full_name CHECK (char_length(btrim(full_name)) BETWEEN 1 AND 100),
    CONSTRAINT chk_users_terms_version CHECK (terms_version ~ '^[A-Za-z0-9._-]{1,32}$')
);

CREATE UNIQUE INDEX ux_users_email ON users (email);
CREATE UNIQUE INDEX ux_users_phone ON users (phone_number) WHERE phone_number IS NOT NULL;
CREATE INDEX idx_users_status_created ON users (status, id DESC);
CREATE INDEX idx_users_purge_pending ON users (deleted_at) WHERE status = 'DELETED' AND purged_at IS NULL;
