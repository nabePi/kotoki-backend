CREATE TABLE learner_profiles (
    user_id                 UUID PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,  -- 1:1 dengan users

    nickname                TEXT NULL,          -- nama panggilan untuk AI (S-4); NULL = AI tidak menyapa nama
    self_declared_level     TEXT NULL,          -- diisi saat onboarding; NULL = dilewati
    target_level            TEXT NULL,
    learning_goal           TEXT NULL,
    learning_goal_note      TEXT NULL,

    preferred_speech_speed  TEXT NOT NULL DEFAULT 'NORMAL',
    preferred_formality     TEXT NOT NULL DEFAULT 'AUTO',
    dialect_preference      TEXT NOT NULL DEFAULT 'STANDARD_ONLY',
    memory_enabled          BOOLEAN NOT NULL DEFAULT true,

    onboarding_completed_at TIMESTAMPTZ NULL,
    created_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at              TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT chk_lp_nickname CHECK (nickname IS NULL OR char_length(nickname) BETWEEN 1 AND 30),
    CONSTRAINT chk_lp_self_declared CHECK (self_declared_level IS NULL OR self_declared_level IN
        ('BEGINNER', 'N5', 'N4', 'N3', 'N2', 'N1')),
    CONSTRAINT chk_lp_target CHECK (target_level IS NULL OR target_level IN ('N5', 'N4', 'N3', 'N2', 'N1')),
    CONSTRAINT chk_lp_goal CHECK (learning_goal IS NULL OR learning_goal IN
        ('DAILY_CONVERSATION', 'JOB_INTERVIEW', 'BUSINESS', 'TRAVEL', 'EXAM_PREP', 'LIVING_IN_JAPAN', 'OTHER')),
    CONSTRAINT chk_lp_goal_note CHECK (learning_goal_note IS NULL OR char_length(learning_goal_note) <= 300),
    CONSTRAINT chk_lp_speed CHECK (preferred_speech_speed IN ('SLOWER', 'NORMAL', 'FASTER')),
    CONSTRAINT chk_lp_formality CHECK (preferred_formality IN ('AUTO', 'CASUAL', 'POLITE')),
    CONSTRAINT chk_lp_dialect CHECK (dialect_preference IN ('STANDARD_ONLY', 'ALLOW_DIALECT'))
);

CREATE TABLE learner_skill_states (
    id                    UUID PRIMARY KEY,
    user_id               UUID NOT NULL REFERENCES learner_profiles(user_id) ON DELETE CASCADE,
    skill                 TEXT NOT NULL,

    prior_level           NUMERIC(3,2) NOT NULL,          -- prior onboarding; dipakai untuk reset & replay
    prior_confidence      NUMERIC(4,3) NOT NULL,          -- confidence awal tabel prior §12.3; titik awal replay §12.5.5 [builder call]
    estimated_level       NUMERIC(3,2) NOT NULL,
    demonstrated_ceiling  NUMERIC(3,2) NULL,              -- max estimated_level yang pernah dicapai dari evidence
    confidence            NUMERIC(4,3) NOT NULL,
    trend                 TEXT NOT NULL DEFAULT 'INSUFFICIENT_DATA',
    evidence_count        INTEGER NOT NULL DEFAULT 0,

    last_evaluation_id    UUID NULL,                      -- FK ditambahkan di 000016
    last_evidence_at      TIMESTAMPTZ NULL,               -- = sessions.ended_at evidence terakhir

    created_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at            TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT ux_learner_skill_states_user_skill UNIQUE (user_id, skill),
    CONSTRAINT chk_lss_skill CHECK (skill IN ('GRAMMAR', 'VOCABULARY', 'FLUENCY', 'NATURALNESS',
        'LISTENING_RESPONSE', 'INTERACTION_COMPLEXITY')),
    CONSTRAINT chk_lss_prior CHECK (prior_level BETWEEN 0 AND 5 AND prior_confidence BETWEEN 0 AND 1),
    CONSTRAINT chk_lss_level CHECK (estimated_level BETWEEN 0 AND 5),
    CONSTRAINT chk_lss_ceiling CHECK (demonstrated_ceiling IS NULL OR demonstrated_ceiling BETWEEN 0 AND 5),
    CONSTRAINT chk_lss_confidence CHECK (confidence BETWEEN 0 AND 1),
    CONSTRAINT chk_lss_trend CHECK (trend IN ('INSUFFICIENT_DATA', 'IMPROVING', 'STABLE', 'DECLINING')),
    CONSTRAINT chk_lss_evidence CHECK (evidence_count >= 0),
    CONSTRAINT chk_lss_evidence_consistency CHECK (
        (evidence_count = 0 AND last_evaluation_id IS NULL AND last_evidence_at IS NULL)
        OR (evidence_count > 0 AND last_evidence_at IS NOT NULL))
);

CREATE TABLE proficiency_estimates (
    id                     UUID PRIMARY KEY,
    user_id                UUID NOT NULL REFERENCES learner_profiles(user_id) ON DELETE CASCADE,
    skill                  TEXT NOT NULL,
    source_evaluation_id   UUID NOT NULL,                  -- FK ditambahkan di 000016
    session_id             UUID NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    algorithm_version      SMALLINT NOT NULL,

    raw_score              NUMERIC(5,2) NOT NULL,          -- skor 0–100 dari evaluation
    effective_difficulty   SMALLINT NULL,                  -- D_eff 1–5
    evidence_level         NUMERIC(3,2) NOT NULL,          -- L_obs
    evidence_confidence    NUMERIC(4,3) NOT NULL,          -- c_obs

    previous_level         NUMERIC(3,2) NOT NULL,
    estimated_level        NUMERIC(3,2) NOT NULL,          -- setelah update
    confidence             NUMERIC(4,3) NOT NULL,          -- setelah update
    evidence_count         INTEGER NOT NULL,               -- setelah update
    trend                  TEXT NOT NULL,                  -- setelah update

    assessed_at            TIMESTAMPTZ NOT NULL,           -- = sessions.ended_at
    created_at             TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT ux_proficiency_estimates_eval_skill UNIQUE (source_evaluation_id, skill),
    CONSTRAINT chk_pe_skill CHECK (skill IN ('GRAMMAR', 'VOCABULARY', 'FLUENCY', 'NATURALNESS',
        'LISTENING_RESPONSE', 'INTERACTION_COMPLEXITY')),
    CONSTRAINT chk_pe_raw CHECK (raw_score BETWEEN 0 AND 100),
    CONSTRAINT chk_pe_deff CHECK (effective_difficulty IS NULL OR effective_difficulty BETWEEN 1 AND 5),
    CONSTRAINT chk_pe_levels CHECK (evidence_level BETWEEN 0 AND 5 AND previous_level BETWEEN 0 AND 5
                                    AND estimated_level BETWEEN 0 AND 5),
    CONSTRAINT chk_pe_conf CHECK (evidence_confidence BETWEEN 0 AND 1 AND confidence BETWEEN 0 AND 1),
    CONSTRAINT chk_pe_count CHECK (evidence_count >= 1),
    CONSTRAINT chk_pe_trend CHECK (trend IN ('INSUFFICIENT_DATA', 'IMPROVING', 'STABLE', 'DECLINING'))
);

CREATE INDEX idx_proficiency_estimates_user_skill_time
    ON proficiency_estimates (user_id, skill, assessed_at DESC);
CREATE INDEX idx_proficiency_estimates_session ON proficiency_estimates (session_id);
