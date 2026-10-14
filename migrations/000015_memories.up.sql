-- 000015_memories.up.sql
CREATE TABLE memories (
    id                            UUID PRIMARY KEY,                -- UUIDv7 dari aplikasi
    user_id                       UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,

    category                      TEXT NOT NULL,
    memory_key                    TEXT NOT NULL,                   -- slot dedup, lihat §11.4

    content                       TEXT NOT NULL,                   -- Jepang; '' hanya untuk tombstone
    display_text                  TEXT NOT NULL,                   -- Indonesia; '' hanya untuk tombstone
    evidence_type                 TEXT NOT NULL,

    confidence                    NUMERIC(4,3) NOT NULL,
    importance                    NUMERIC(4,3) NOT NULL,
    observation_count             INTEGER NOT NULL DEFAULT 1,

    source_session_id             UUID NULL REFERENCES sessions(id) ON DELETE SET NULL,
    source_turn_ids               UUID[] NOT NULL DEFAULT '{}',    -- informatif; turn bisa terhapus oleh retensi transcript
    extraction_prompt_version_id  UUID NULL REFERENCES prompt_versions(id),

    embedding                     vector(1536) NULL,
    embedding_model               TEXT NULL,

    status                        TEXT NOT NULL DEFAULT 'ACTIVE',
    superseded_by                 UUID NULL REFERENCES memories(id) ON DELETE SET NULL
                                      DEFERRABLE INITIALLY DEFERRED,

    first_observed_at             TIMESTAMPTZ NOT NULL,
    last_observed_at              TIMESTAMPTZ NOT NULL,
    expires_at                    TIMESTAMPTZ NOT NULL,

    created_at                    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at                    TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT chk_memories_category CHECK (category IN
        ('PROFILE_FACT', 'PREFERENCE', 'LEARNING_PATTERN', 'EPISODIC')),
    CONSTRAINT chk_memories_key_fmt CHECK (memory_key ~ '^[a-z0-9_]+(:[a-z0-9_-]+){0,2}$'
                                           AND char_length(memory_key) <= 100),
    CONSTRAINT chk_memories_evidence CHECK (evidence_type IN
        ('EXPLICIT_USER_STATEMENT', 'USER_CONFIRMED', 'INFERRED', 'USER_DELETED')),
    CONSTRAINT chk_memories_confidence CHECK (confidence BETWEEN 0 AND 1),
    CONSTRAINT chk_memories_importance CHECK (importance BETWEEN 0 AND 1),
    CONSTRAINT chk_memories_obs CHECK (observation_count >= 1),
    CONSTRAINT chk_memories_status CHECK (status IN ('ACTIVE', 'SUPERSEDED', 'DELETED')),
    CONSTRAINT chk_memories_superseded_by CHECK (status = 'SUPERSEDED' OR superseded_by IS NULL),
    CONSTRAINT chk_memories_content CHECK (
        (status = 'DELETED' AND content = '' AND display_text = '' AND embedding IS NULL
             AND evidence_type = 'USER_DELETED')
        OR
        (status <> 'DELETED' AND evidence_type <> 'USER_DELETED'
             AND char_length(content) BETWEEN 1 AND 200
             AND char_length(display_text) BETWEEN 1 AND 400)
    ),
    CONSTRAINT chk_memories_observed CHECK (last_observed_at >= first_observed_at)
);

-- Dedup slot: maksimal satu ACTIVE per (user, category, key).
CREATE UNIQUE INDEX ux_memories_active_slot
    ON memories (user_id, category, memory_key) WHERE status = 'ACTIVE';

-- Tombstone: maksimal satu per slot.
CREATE UNIQUE INDEX ux_memories_tombstone_slot
    ON memories (user_id, category, memory_key) WHERE status = 'DELETED';

-- Retrieval & listing per user (semua query dimulai dari user_id).
CREATE INDEX idx_memories_user_status_cat
    ON memories (user_id, status, category, last_observed_at DESC);

-- Cleanup retensi.
CREATE INDEX idx_memories_expires_at ON memories (expires_at);

-- Pendukung FK ON DELETE SET NULL.
CREATE INDEX idx_memories_source_session ON memories (source_session_id) WHERE source_session_id IS NOT NULL;
CREATE INDEX idx_memories_superseded_by  ON memories (superseded_by)     WHERE superseded_by IS NOT NULL;
