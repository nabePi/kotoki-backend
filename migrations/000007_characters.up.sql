CREATE TABLE characters (
    id            UUID        PRIMARY KEY,
    code          TEXT        NOT NULL,
    status        TEXT        NOT NULL DEFAULT 'ACTIVE',
    sort_order    INTEGER     NOT NULL DEFAULT 0,
    name          TEXT        NOT NULL,
    avatar_key    TEXT,
    persona       TEXT        NOT NULL,
    greeting_hint TEXT,
    provider_name TEXT        NOT NULL,   -- TTS utama (D-015)
    model_name    TEXT        NOT NULL,
    voice_id      TEXT        NOT NULL,
    voice         JSONB       NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT ux_characters_code        UNIQUE (code),
    CONSTRAINT chk_characters_code       CHECK (code ~ '^[A-Z][A-Z0-9_]{1,49}$'),
    CONSTRAINT chk_characters_status     CHECK (status IN ('ACTIVE','DISABLED','ARCHIVED')),
    CONSTRAINT chk_characters_sort       CHECK (sort_order >= 0),
    CONSTRAINT chk_characters_name       CHECK (char_length(name) BETWEEN 1 AND 50),
    CONSTRAINT chk_characters_avatar     CHECK (avatar_key IS NULL OR char_length(avatar_key) BETWEEN 1 AND 100),
    CONSTRAINT chk_characters_persona    CHECK (char_length(persona) BETWEEN 50 AND 3000),
    CONSTRAINT chk_characters_greeting   CHECK (greeting_hint IS NULL OR char_length(greeting_hint) BETWEEN 1 AND 200),
    CONSTRAINT chk_characters_provider   CHECK (provider_name IN ('ELEVENLABS','CARTESIA')),
    CONSTRAINT chk_characters_model      CHECK (char_length(btrim(model_name)) BETWEEN 1 AND 200),
    CONSTRAINT chk_characters_voice_id   CHECK (char_length(btrim(voice_id)) BETWEEN 1 AND 200),
    -- Bentuk dasar voice (isi lengkap divalidasi Go content.Validator, §6.10.1):
    CONSTRAINT chk_characters_voice_obj  CHECK (jsonb_typeof(voice) = 'object'),
    CONSTRAINT chk_characters_voice_gender CHECK (COALESCE(voice->>'gender', '') IN ('FEMALE','MALE')),
    CONSTRAINT chk_characters_voice_accent CHECK (COALESCE(voice->>'accent', '') IN ('KANTO','KANSAI')),
    CONSTRAINT chk_characters_voice_rate CHECK (
        CASE WHEN jsonb_typeof(voice->'speaking_rate') = 'number'
             THEN (voice->>'speaking_rate')::numeric BETWEEN 0.80 AND 1.20
             ELSE false END),
    CONSTRAINT chk_characters_voice_fallback CHECK (NOT (voice ? 'fallback') OR jsonb_typeof(voice->'fallback') = 'object')
);

CREATE INDEX idx_characters_status_sort ON characters (status, sort_order);
