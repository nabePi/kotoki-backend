CREATE TABLE sessions (
    id                            UUID PRIMARY KEY,
    user_id                       UUID NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    status                        TEXT NOT NULL,
    end_reason                    TEXT NULL,
    conversation_start_mode       TEXT NOT NULL,
    duration_seconds              INTEGER NOT NULL,

    topic_id                      UUID NOT NULL REFERENCES topics(id) ON DELETE RESTRICT,
    scenario_id                   UUID NOT NULL REFERENCES scenarios(id) ON DELETE RESTRICT,
    scenario_version_id           UUID NOT NULL REFERENCES scenario_versions(id) ON DELETE RESTRICT,
    character_id                  UUID NOT NULL REFERENCES characters(id) ON DELETE RESTRICT,
    prompt_version_id             UUID NOT NULL REFERENCES prompt_versions(id) ON DELETE RESTRICT, -- key CONVERSATION
    opening_prompt_version_id     UUID NULL REFERENCES prompt_versions(id) ON DELETE RESTRICT,     -- key OPENING; diisi saat STARTING (AI_FIRST)
    config_snapshot               JSONB NOT NULL,         -- snapshot karakter + formalitas (D-014), immutable; diisi saat POST /sessions
    adaptive_difficulty           JSONB NOT NULL,         -- snapshot AdaptiveDifficulty (Bab 12 §12.8.2), immutable
    runtime_fence                 BIGINT NOT NULL DEFAULT 0, -- fencing runtime (D-084, Bab 7 §7.9.3)

    reserved_seconds              INTEGER NOT NULL,
    charged_seconds               INTEGER NOT NULL DEFAULT 0,
    disconnected_seconds          INTEGER NOT NULL DEFAULT 0,
    failure_detail                TEXT NULL,              -- kode/teks pendek internal, mis. 'stt_connect_timeout'
    summary_text                  TEXT NULL,              -- diisi summary worker (Bab 9 §9.9.6); NULL-kan oleh retensi 180 hari
    transcript_purged_at          TIMESTAMPTZ NULL,       -- diisi job transcript_retention saat turn sesi ini dihapus (Bab 9 §9.9.4)

    create_idempotency_key        TEXT NOT NULL,
    create_request_hash           BYTEA NOT NULL,         -- sha256 canonical request

    created_at                    TIMESTAMPTZ NOT NULL DEFAULT now(),
    starting_at                   TIMESTAMPTZ NULL,
    started_at                    TIMESTAMPTZ NULL,
    expires_at                    TIMESTAMPTZ NULL,
    ending_at                     TIMESTAMPTZ NULL,
    ended_at                      TIMESTAMPTZ NULL,
    handoff_at                    TIMESTAMPTZ NULL,       -- diisi runtime saat DRAIN_RELEASE (MarkHandoff), NULL-kan oleh ClaimRuntime (S-10)
    updated_at                    TIMESTAMPTZ NOT NULL DEFAULT now(),
    version                       BIGINT NOT NULL DEFAULT 1,

    CONSTRAINT chk_sessions_status CHECK (status IN
        ('RESERVED','STARTING','ACTIVE','ENDING','COMPLETED','CANCELLED','ABANDONED','FAILED')),
    CONSTRAINT chk_sessions_end_reason CHECK (end_reason IS NULL OR end_reason IN
        ('USER_ENDED','DURATION_ELAPSED','RECONNECT_TIMEOUT','PROVIDER_FAILURE','SERVER_ERROR',
         'USER_CANCELLED','NOT_STARTED')),
    CONSTRAINT chk_sessions_start_mode CHECK (conversation_start_mode IN ('USER_FIRST','AI_FIRST')),
    CONSTRAINT chk_sessions_duration CHECK (duration_seconds IN (300,600,900,1800,2700,3600)),
    CONSTRAINT chk_sessions_reserved CHECK (reserved_seconds = duration_seconds),
    CONSTRAINT chk_sessions_charged CHECK (charged_seconds BETWEEN 0 AND reserved_seconds),
    CONSTRAINT chk_sessions_disconnected CHECK (disconnected_seconds BETWEEN 0 AND duration_seconds),
    CONSTRAINT chk_sessions_adaptive CHECK (jsonb_typeof(adaptive_difficulty) = 'object'),
    CONSTRAINT chk_sessions_config_snapshot CHECK (jsonb_typeof(config_snapshot) = 'object'),
    CONSTRAINT chk_sessions_runtime_fence CHECK (runtime_fence >= 0),
    CONSTRAINT chk_sessions_opening_prompt CHECK (
        opening_prompt_version_id IS NULL
        OR (conversation_start_mode = 'AI_FIRST' AND status <> 'RESERVED')),
    CONSTRAINT chk_sessions_idem_key CHECK (create_idempotency_key ~ '^[A-Za-z0-9_-]{8,128}$'),
    CONSTRAINT chk_sessions_expires CHECK (
        (started_at IS NULL AND expires_at IS NULL)
        OR expires_at = started_at + make_interval(secs => duration_seconds)),
    CONSTRAINT chk_sessions_state CHECK (
        (status = 'RESERVED'  AND end_reason IS NULL AND starting_at IS NULL AND started_at IS NULL
                              AND ending_at IS NULL AND ended_at IS NULL)
     OR (status = 'STARTING'  AND end_reason IS NULL AND starting_at IS NOT NULL AND started_at IS NULL
                              AND ending_at IS NULL AND ended_at IS NULL)
     OR (status = 'ACTIVE'    AND end_reason IS NULL AND started_at IS NOT NULL
                              AND ending_at IS NULL AND ended_at IS NULL)
     OR (status = 'ENDING'    AND end_reason IN ('USER_ENDED','DURATION_ELAPSED','RECONNECT_TIMEOUT',
                                                 'PROVIDER_FAILURE','SERVER_ERROR')
                              AND started_at IS NOT NULL AND ending_at IS NOT NULL AND ended_at IS NULL)
     OR (status = 'COMPLETED' AND end_reason IN ('USER_ENDED','DURATION_ELAPSED','RECONNECT_TIMEOUT')
                              AND started_at IS NOT NULL AND ending_at IS NOT NULL AND ended_at IS NOT NULL)
     OR (status = 'FAILED'    AND end_reason IN ('PROVIDER_FAILURE','SERVER_ERROR') AND ended_at IS NOT NULL
                              AND starting_at IS NOT NULL)
     OR (status = 'CANCELLED' AND end_reason = 'USER_CANCELLED' AND started_at IS NULL AND ended_at IS NOT NULL
                              AND charged_seconds = 0)
     OR (status = 'ABANDONED' AND end_reason = 'NOT_STARTED' AND started_at IS NULL AND ended_at IS NOT NULL
                              AND charged_seconds = 0)
    ),
    CONSTRAINT chk_sessions_failed_unstarted_free CHECK (
        NOT (status = 'FAILED' AND started_at IS NULL AND charged_seconds <> 0)),
    CONSTRAINT chk_sessions_handoff CHECK (
        handoff_at IS NULL OR (started_at IS NOT NULL AND handoff_at >= started_at))
);

CREATE UNIQUE INDEX ux_sessions_user_idempotency ON sessions (user_id, create_idempotency_key);
CREATE UNIQUE INDEX ux_sessions_one_open_per_user ON sessions (user_id)
    WHERE status IN ('RESERVED','STARTING','ACTIVE','ENDING');
CREATE INDEX idx_sessions_user_id_desc     ON sessions (user_id, id DESC);
CREATE INDEX idx_sessions_active_expires   ON sessions (expires_at)  WHERE status = 'ACTIVE';
CREATE INDEX idx_sessions_starting         ON sessions (starting_at) WHERE status = 'STARTING';
CREATE INDEX idx_sessions_ending           ON sessions (ending_at)   WHERE status = 'ENDING';
CREATE INDEX idx_sessions_handoff          ON sessions (handoff_at)  WHERE status = 'ACTIVE' AND handoff_at IS NOT NULL;
CREATE INDEX idx_sessions_retention        ON sessions (ended_at)
    WHERE ended_at IS NOT NULL AND transcript_purged_at IS NULL;
CREATE INDEX idx_sessions_summary_retention ON sessions (ended_at) WHERE summary_text IS NOT NULL;
