-- migrations/000016_evaluations.up.sql
CREATE TABLE evaluations (
    id                              UUID        PRIMARY KEY,           -- UUIDv7
    session_id                      UUID        NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    user_id                         UUID        NOT NULL REFERENCES users(id)    ON DELETE CASCADE,

    status                          TEXT        NOT NULL,
    insufficient_evidence           BOOLEAN     NOT NULL DEFAULT false,

    grammar_score                   SMALLINT,
    vocabulary_score                SMALLINT,
    fluency_score                   SMALLINT,
    naturalness_score               SMALLINT,
    task_achievement_score          SMALLINT,
    overall_score                   NUMERIC(5,2),

    -- Sinyal skill tambahan (R-13; dibaca Bab 12 §12.4). Tidak masuk overall, tidak ditampilkan.
    listening_response_signal       SMALLINT    NULL,                          -- 0–100
    interaction_complexity_signal   SMALLINT    NULL,                          -- 0–100
    dimension_confidence            JSONB       NOT NULL DEFAULT '{}'::jsonb,  -- {"GRAMMAR":0.82,...,"LISTENING_RESPONSE":0.7}
    dimension_comments              JSONB       NOT NULL DEFAULT '{}'::jsonb,  -- {"GRAMMAR":"<id>",...}

    summary_feedback                TEXT,                                      -- bahasa Indonesia
    strengths                       JSONB       NOT NULL DEFAULT '[]'::jsonb,  -- array string (id)
    focus_areas                     JSONB       NOT NULL DEFAULT '[]'::jsonb,  -- array string (id)
    recommendations                 JSONB       NOT NULL DEFAULT '[]'::jsonb,  -- array {title, detail, example_ja}

    user_turn_count                 INTEGER,
    user_speech_seconds             INTEGER,
    active_seconds                  INTEGER,
    scenario_difficulty_level       SMALLINT,

    rubric_version                  TEXT        NOT NULL,
    prompt_version_id               UUID        REFERENCES prompt_versions(id),  -- key EVALUATION
    provider                        TEXT,                                      -- provider.Name UPPERCASE (R-6), mis. 'OPENAI'; vendor yang menghasilkan output valid
    model                           TEXT,
    llm_call_count                  SMALLINT    NOT NULL DEFAULT 0,            -- jumlah panggilan GenerateJSON (1–2, §10.6.1)
    input_tokens                    INTEGER,
    output_tokens                   INTEGER,

    attempt_count                   INTEGER     NOT NULL DEFAULT 0,
    failure_code                    TEXT,
    last_error                      TEXT,
    source_event_id                 UUID        NOT NULL,                       -- event session.completed

    details_purged_at               TIMESTAMPTZ,                                -- retensi D-099 (Bab 9 §9.9.4)
    created_at                      TIMESTAMPTZ NOT NULL DEFAULT now(),
    started_at                      TIMESTAMPTZ,
    completed_at                    TIMESTAMPTZ,
    updated_at                      TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT uq_evaluations_session UNIQUE (session_id),
    CONSTRAINT chk_evaluations_status CHECK (status IN ('PENDING', 'RUNNING', 'COMPLETED', 'FAILED')),
    CONSTRAINT chk_evaluations_scores CHECK (
        (grammar_score          IS NULL OR grammar_score          BETWEEN 0 AND 100) AND
        (vocabulary_score       IS NULL OR vocabulary_score       BETWEEN 0 AND 100) AND
        (fluency_score          IS NULL OR fluency_score          BETWEEN 0 AND 100) AND
        (naturalness_score      IS NULL OR naturalness_score      BETWEEN 0 AND 100) AND
        (task_achievement_score IS NULL OR task_achievement_score BETWEEN 0 AND 100) AND
        (overall_score          IS NULL OR overall_score          BETWEEN 0 AND 100) AND
        (listening_response_signal     IS NULL OR listening_response_signal     BETWEEN 0 AND 100) AND
        (interaction_complexity_signal IS NULL OR interaction_complexity_signal BETWEEN 0 AND 100)),
    CONSTRAINT chk_evaluations_failure_code CHECK (
        failure_code IS NULL OR failure_code IN
        ('TRANSCRIPT_UNAVAILABLE', 'PROVIDER_UNAVAILABLE', 'INVALID_MODEL_OUTPUT', 'CONFIGURATION_ERROR', 'INTERNAL_ERROR')),
    -- COMPLETED normal: 5 dimensi LLM + overall wajib terisi
    CONSTRAINT chk_evaluations_completed_scores CHECK (
        status <> 'COMPLETED' OR insufficient_evidence OR (
            grammar_score IS NOT NULL AND vocabulary_score IS NOT NULL AND fluency_score IS NOT NULL AND
            naturalness_score IS NOT NULL AND task_achievement_score IS NOT NULL AND overall_score IS NOT NULL)),
    -- insufficient evidence: semua skor null
    CONSTRAINT chk_evaluations_insufficient CHECK (
        NOT insufficient_evidence OR (
            status = 'COMPLETED' AND grammar_score IS NULL AND vocabulary_score IS NULL AND
            fluency_score IS NULL AND naturalness_score IS NULL AND task_achievement_score IS NULL AND
            overall_score IS NULL)),
    CONSTRAINT chk_evaluations_completed_at CHECK ((status = 'COMPLETED') = (completed_at IS NOT NULL)),
    CONSTRAINT chk_evaluations_failed CHECK (status <> 'FAILED' OR failure_code IS NOT NULL),
    CONSTRAINT chk_evaluations_attempts CHECK (attempt_count >= 0)
);

CREATE INDEX idx_evaluations_user_completed ON evaluations (user_id, completed_at DESC) WHERE status = 'COMPLETED';
CREATE INDEX idx_evaluations_status_updated ON evaluations (status, updated_at) WHERE status IN ('PENDING', 'RUNNING', 'FAILED');
CREATE INDEX idx_evaluations_retention ON evaluations (completed_at) WHERE status = 'COMPLETED' AND details_purged_at IS NULL;

CREATE TABLE evaluation_findings (
    id                    UUID        PRIMARY KEY,                       -- UUIDv7
    evaluation_id         UUID        NOT NULL REFERENCES evaluations(id) ON DELETE CASCADE,
    session_id            UUID        NOT NULL REFERENCES sessions(id)    ON DELETE CASCADE,
    user_id               UUID        NOT NULL REFERENCES users(id)       ON DELETE CASCADE,
    session_turn_id       UUID        NULL,                              -- TANPA FK: session_turns dipartisi (S-6);
                                                                         -- integritas dijaga aplikasi [builder call]
    turn_sequence_number  INTEGER,
    ordinal               SMALLINT    NOT NULL,                          -- urutan tampil, mulai 1
    dimension             TEXT        NOT NULL,
    finding_type          TEXT        NOT NULL,
    severity              TEXT        NOT NULL,
    original_text         TEXT,                                          -- kutipan ujaran user (Jepang)
    corrected_text        TEXT,                                          -- koreksi (Jepang)
    explanation           TEXT        NOT NULL,                          -- bahasa Indonesia
    pattern_key           TEXT,                                          -- mis. 'particle.de_ni_location'
    confidence            NUMERIC(3,2) NOT NULL,
    source                TEXT        NOT NULL,                          -- LLM
    created_at            TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT uq_evaluation_findings_ordinal UNIQUE (evaluation_id, ordinal),
    CONSTRAINT chk_ef_dimension CHECK (dimension IN
        ('GRAMMAR', 'VOCABULARY', 'FLUENCY', 'NATURALNESS', 'TASK_ACHIEVEMENT')),
    CONSTRAINT chk_ef_severity CHECK (severity IN ('INFO', 'MINOR', 'MAJOR')),
    CONSTRAINT chk_ef_source CHECK (source IN ('LLM')),
    CONSTRAINT chk_ef_confidence CHECK (confidence BETWEEN 0 AND 1),
    CONSTRAINT chk_ef_type_dimension CHECK (
        CASE finding_type
            WHEN 'PARTICLE_ERROR'           THEN dimension = 'GRAMMAR'
            WHEN 'CONJUGATION_ERROR'        THEN dimension = 'GRAMMAR'
            WHEN 'TENSE_ASPECT_ERROR'       THEN dimension = 'GRAMMAR'
            WHEN 'SENTENCE_STRUCTURE_ERROR' THEN dimension = 'GRAMMAR'
            WHEN 'WORD_CHOICE_ERROR'        THEN dimension = 'VOCABULARY'
            WHEN 'NON_JAPANESE_WORD'        THEN dimension = 'VOCABULARY'
            WHEN 'COLLOCATION_ERROR'        THEN dimension = 'VOCABULARY'
            WHEN 'POLITENESS_MISMATCH'      THEN dimension = 'NATURALNESS'
            WHEN 'UNNATURAL_EXPRESSION'     THEN dimension = 'NATURALNESS'
            WHEN 'EXCESSIVE_FILLER'         THEN dimension = 'FLUENCY'
            WHEN 'FRAGMENTED_UTTERANCE'     THEN dimension = 'FLUENCY'
            WHEN 'OVERLY_SHORT_RESPONSE'    THEN dimension = 'FLUENCY'
            WHEN 'OFF_TOPIC_RESPONSE'       THEN dimension = 'TASK_ACHIEVEMENT'
            WHEN 'MISUNDERSTOOD_PROMPT'     THEN dimension = 'TASK_ACHIEVEMENT'
            WHEN 'OBJECTIVE_NOT_MET'        THEN dimension = 'TASK_ACHIEVEMENT'
            WHEN 'STRENGTH'                 THEN severity = 'INFO'
            ELSE false
        END),
    CONSTRAINT chk_ef_lengths CHECK (
        (original_text  IS NULL OR length(original_text)  <= 300) AND
        (corrected_text IS NULL OR length(corrected_text) <= 300) AND
        length(explanation) BETWEEN 1 AND 600 AND
        (pattern_key IS NULL OR pattern_key ~ '^[a-z0-9_.]{3,64}$'))
);

CREATE INDEX idx_evaluation_findings_evaluation ON evaluation_findings (evaluation_id, ordinal);
CREATE INDEX idx_evaluation_findings_user_pattern ON evaluation_findings (user_id, pattern_key, created_at DESC)
    WHERE pattern_key IS NOT NULL;
-- Tidak ada index pada session_turn_id: tanpa FK (S-6) tidak ada query/cascade yang mencarinya.

-- FK dari tabel learner (migration 000014 lebih dulu dari 000016; Bab 12 §12.2.4)
ALTER TABLE learner_skill_states
    ADD CONSTRAINT fk_lss_last_evaluation FOREIGN KEY (last_evaluation_id)
    REFERENCES evaluations(id) ON DELETE SET NULL;
ALTER TABLE proficiency_estimates
    ADD CONSTRAINT fk_pe_source_evaluation FOREIGN KEY (source_evaluation_id)
    REFERENCES evaluations(id) ON DELETE CASCADE;
CREATE INDEX idx_lss_last_evaluation ON learner_skill_states (last_evaluation_id) WHERE last_evaluation_id IS NOT NULL;
