CREATE TABLE session_turns (
    id                          UUID NOT NULL,            -- UUIDv7; PK gabungan (id, created_at) karena partitioning
    session_id                  UUID NOT NULL REFERENCES sessions(id) ON DELETE RESTRICT,
    sequence_number             INTEGER NOT NULL,
    status                      TEXT NULL,               -- NULL = turn sedang berjalan
    user_text                   TEXT NULL,               -- NULL untuk opening AI_FIRST (seq 0)
    user_started_at             TIMESTAMPTZ NULL,
    user_ended_at               TIMESTAMPTZ NULL,
    assistant_text              TEXT NULL,               -- teks penuh yang dihasilkan LLM
    assistant_spoken_text       TEXT NULL,               -- kalimat yang audionya selesai terkirim (D-078)
    generation                  INTEGER NOT NULL,        -- generation AI (D-073)
    stt_provider                TEXT NULL,               -- identifier UPPERCASE D-040/D-017: OPENAI, ELEVENLABS, CARTESIA, FAKE_PRIMARY, …
    llm_provider                TEXT NULL,               -- OPENAI, DEEPSEEK, FAKE_PRIMARY
    llm_model                   TEXT NULL,
    tts_provider                TEXT NULL,               -- ELEVENLABS, CARTESIA, FAKE_*
    tts_model                   TEXT NULL,
    tts_voice_id                TEXT NULL,               -- voice ID vendor aktual (dari `config_snapshot.character.voice_id` atau `voice.fallback.voice_id`) yang dipakai
    used_fallback               BOOLEAN NOT NULL DEFAULT false,  -- true hanya bila TTS memakai `voice.fallback` (D-017: tanpa fallback STT/LLM)
    turn_end_to_first_audio_ms  INTEGER NULL,
    stt_final_ms                INTEGER NULL,
    llm_first_token_ms          INTEGER NULL,
    tts_first_audio_ms          INTEGER NULL,
    input_tokens                INTEGER NULL,
    output_tokens               INTEGER NULL,
    tts_characters              INTEGER NULL,
    stt_audio_ms                INTEGER NULL,
    error_code                  TEXT NULL,               -- diisi jika FAILED (kode dari Bab 7/8)
    created_at                  TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at                TIMESTAMPTZ NULL,

    CONSTRAINT pk_session_turns PRIMARY KEY (id, created_at),
    CONSTRAINT ux_session_turns_seq UNIQUE (session_id, sequence_number, created_at),
        -- unik global (session_id, sequence_number) dijamin aplikasi: satu owner ber-fence (D-084), seq naik tanpa celah
    CONSTRAINT chk_turn_status CHECK (status IS NULL OR status IN ('COMPLETED','INTERRUPTED','FAILED','DISCARDED')),
    CONSTRAINT chk_turn_completed CHECK ((status IS NULL) = (completed_at IS NULL)),
    CONSTRAINT chk_turn_seq CHECK (sequence_number >= 0),
    CONSTRAINT chk_turn_opening CHECK (sequence_number > 0 OR user_text IS NULL),
    CONSTRAINT chk_turn_generation CHECK (generation >= 0),
    CONSTRAINT chk_turn_metrics CHECK (
        COALESCE(turn_end_to_first_audio_ms, 0) >= 0 AND COALESCE(stt_final_ms, 0) >= 0
        AND COALESCE(llm_first_token_ms, 0) >= 0 AND COALESCE(tts_first_audio_ms, 0) >= 0
        AND COALESCE(input_tokens, 0) >= 0 AND COALESCE(output_tokens, 0) >= 0
        AND COALESCE(tts_characters, 0) >= 0 AND COALESCE(stt_audio_ms, 0) >= 0),
    CONSTRAINT chk_turn_provider_upper CHECK (
        COALESCE(stt_provider, 'X') ~ '^[A-Z][A-Z0-9_]*$' AND COALESCE(llm_provider, 'X') ~ '^[A-Z][A-Z0-9_]*$'
        AND COALESCE(tts_provider, 'X') ~ '^[A-Z][A-Z0-9_]*$')
) PARTITION BY RANGE (created_at);

-- Index pada tabel partitioned = index lokal per partisi (dibuat otomatis di partisi baru).
-- Lookup per sesi memakai ux_session_turns_seq (prefix session_id, sequence_number).
CREATE INDEX idx_session_turns_open ON session_turns (session_id) WHERE status IS NULL;
CREATE INDEX idx_session_turns_created ON session_turns (created_at);   -- job transcript_retention (partisi batas)

-- Partisi awal: bulan berjalan + 3 bulan ke depan (UTC). Nama: session_turns_pYYYYMM.
DO $$
DECLARE m date := date_trunc('month', now() AT TIME ZONE 'UTC')::date;
BEGIN
  FOR i IN 0..3 LOOP
    EXECUTE format(
      'CREATE TABLE IF NOT EXISTS %I PARTITION OF session_turns FOR VALUES FROM (%L) TO (%L)',
      'session_turns_p' || to_char(m + make_interval(months => i), 'YYYYMM'),
      (m + make_interval(months => i))::timestamp AT TIME ZONE 'UTC',
      (m + make_interval(months => i + 1))::timestamp AT TIME ZONE 'UTC');
  END LOOP;
END $$;
