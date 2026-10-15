
-- Guard generik untuk tabel *_versions (dipakai 000006 untuk scenario_versions dan 000008 untuk prompt_versions).
-- 1) Kolom isi tidak boleh berubah (status apa pun). Hanya status/published_at/retired_at yang boleh berubah.
-- 2) Transisi status yang sah: DRAFT->PUBLISHED, PUBLISHED->RETIRED, RETIRED->PUBLISHED.
-- 3) DELETE hanya untuk DRAFT.
CREATE OR REPLACE FUNCTION content_version_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    mutable_cols CONSTANT TEXT[] := ARRAY['status','published_at','retired_at'];
BEGIN
    IF TG_OP = 'DELETE' THEN
        IF OLD.status <> 'DRAFT' THEN
            RAISE EXCEPTION 'content version %.% is %; only DRAFT can be deleted', TG_TABLE_NAME, OLD.id, OLD.status
                USING ERRCODE = 'check_violation';
        END IF;
        RETURN OLD;
    END IF;

    IF (to_jsonb(NEW) - mutable_cols) IS DISTINCT FROM (to_jsonb(OLD) - mutable_cols) THEN
        RAISE EXCEPTION 'content version %.% is immutable', TG_TABLE_NAME, OLD.id
            USING ERRCODE = 'check_violation';
    END IF;

    IF NEW.status <> OLD.status AND NOT (
           (OLD.status = 'DRAFT'     AND NEW.status = 'PUBLISHED')
        OR (OLD.status = 'PUBLISHED' AND NEW.status = 'RETIRED')
        OR (OLD.status = 'RETIRED'   AND NEW.status = 'PUBLISHED')
    ) THEN
        RAISE EXCEPTION 'invalid content version transition % -> % on %.%', OLD.status, NEW.status, TG_TABLE_NAME, OLD.id
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TABLE scenarios (
    id          UUID        PRIMARY KEY,
    topic_id    UUID        NOT NULL,
    code        TEXT        NOT NULL,
    name        TEXT        NOT NULL,
    name_ja     TEXT        NOT NULL,
    description TEXT        NOT NULL DEFAULT '',
    sort_order  INTEGER     NOT NULL DEFAULT 0,
    status      TEXT        NOT NULL DEFAULT 'ACTIVE',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT fk_scenarios_topic FOREIGN KEY (topic_id) REFERENCES topics (id) ON DELETE RESTRICT,
    CONSTRAINT ux_scenarios_code        UNIQUE (code),
    CONSTRAINT chk_scenarios_code       CHECK (code ~ '^[A-Z][A-Z0-9_]{1,79}$'),
    CONSTRAINT chk_scenarios_name       CHECK (char_length(name) BETWEEN 1 AND 150),
    CONSTRAINT chk_scenarios_name_ja    CHECK (char_length(name_ja) BETWEEN 1 AND 150),
    CONSTRAINT chk_scenarios_desc       CHECK (char_length(description) <= 1000),
    CONSTRAINT chk_scenarios_sort_order CHECK (sort_order >= 0),
    CONSTRAINT chk_scenarios_status     CHECK (status IN ('ACTIVE','DISABLED','ARCHIVED'))
);

CREATE INDEX idx_scenarios_topic_status_sort ON scenarios (topic_id, status, sort_order);

CREATE TABLE scenario_versions (
    id                    UUID         PRIMARY KEY,
    scenario_id           UUID         NOT NULL,
    version               INTEGER      NOT NULL,
    status                TEXT         NOT NULL DEFAULT 'DRAFT',

    difficulty_level      SMALLINT     NOT NULL,
    recommended_level_min NUMERIC(2,1) NOT NULL,
    recommended_level_max NUMERIC(2,1) NOT NULL,

    situation             TEXT         NOT NULL,
    ai_role               TEXT         NOT NULL,
    user_role             TEXT         NOT NULL,
    goal                  TEXT         NOT NULL,
    tasks                 JSONB        NOT NULL DEFAULT '[]'::jsonb,
    constraints           JSONB        NOT NULL DEFAULT '[]'::jsonb,
    grammar_targets       JSONB        NOT NULL DEFAULT '[]'::jsonb,
    vocab_hints           JSONB        NOT NULL DEFAULT '[]'::jsonb,
    evaluation_focus      TEXT[]       NOT NULL DEFAULT '{}',
    formality             TEXT         NOT NULL DEFAULT 'POLITE',
    opening_line          TEXT         NOT NULL,
    visual_prompt         JSONB        NOT NULL,
    notes                 TEXT,

    created_at            TIMESTAMPTZ  NOT NULL DEFAULT now(),
    published_at          TIMESTAMPTZ,
    retired_at            TIMESTAMPTZ,

    CONSTRAINT fk_scenario_versions_scenario FOREIGN KEY (scenario_id) REFERENCES scenarios (id) ON DELETE RESTRICT,
    CONSTRAINT ux_scenario_versions_version  UNIQUE (scenario_id, version),
    CONSTRAINT chk_scenario_versions_version CHECK (version > 0),
    CONSTRAINT chk_scenario_versions_status  CHECK (status IN ('DRAFT','PUBLISHED','RETIRED')),
    CONSTRAINT chk_scenario_versions_status_ts CHECK (
           (status = 'DRAFT'     AND published_at IS NULL     AND retired_at IS NULL)
        OR (status = 'PUBLISHED' AND published_at IS NOT NULL AND retired_at IS NULL)
        OR (status = 'RETIRED'   AND published_at IS NOT NULL AND retired_at IS NOT NULL)),
    CONSTRAINT chk_scenario_versions_difficulty CHECK (difficulty_level BETWEEN 1 AND 5),
    CONSTRAINT chk_scenario_versions_levels CHECK (
        recommended_level_min BETWEEN 0.0 AND 5.0
        AND recommended_level_max BETWEEN 0.0 AND 5.0
        AND recommended_level_min <= recommended_level_max),
    CONSTRAINT chk_scenario_versions_situation CHECK (char_length(situation) BETWEEN 1 AND 2000),
    CONSTRAINT chk_scenario_versions_ai_role   CHECK (char_length(ai_role) BETWEEN 1 AND 300),
    CONSTRAINT chk_scenario_versions_user_role CHECK (char_length(user_role) BETWEEN 1 AND 300),
    CONSTRAINT chk_scenario_versions_goal      CHECK (char_length(goal) BETWEEN 1 AND 500),
    CONSTRAINT chk_scenario_versions_tasks     CHECK (jsonb_typeof(tasks) = 'array' AND jsonb_array_length(tasks) <= 6),
    CONSTRAINT chk_scenario_versions_constraints CHECK (jsonb_typeof(constraints) = 'array' AND jsonb_array_length(constraints) <= 8),
    CONSTRAINT chk_scenario_versions_grammar   CHECK (jsonb_typeof(grammar_targets) = 'array' AND jsonb_array_length(grammar_targets) <= 8),
    CONSTRAINT chk_scenario_versions_vocab     CHECK (jsonb_typeof(vocab_hints) = 'array' AND jsonb_array_length(vocab_hints) <= 20),
    CONSTRAINT chk_scenario_versions_evalfocus CHECK (
        cardinality(evaluation_focus) <= 3
        AND evaluation_focus <@ ARRAY['GRAMMAR','VOCABULARY','FLUENCY','NATURALNESS',
                                      'LISTENING_RESPONSE','INTERACTION_COMPLEXITY']::TEXT[]),
    CONSTRAINT chk_scenario_versions_formality CHECK (formality IN ('CASUAL','POLITE','KEIGO')),
    CONSTRAINT chk_scenario_versions_opening   CHECK (char_length(opening_line) BETWEEN 1 AND 200),
    CONSTRAINT chk_scenario_versions_visual    CHECK (jsonb_typeof(visual_prompt) = 'object'),
    CONSTRAINT chk_scenario_versions_notes     CHECK (notes IS NULL OR char_length(notes) <= 1000)
);

CREATE UNIQUE INDEX ux_scenario_versions_published
    ON scenario_versions (scenario_id) WHERE status = 'PUBLISHED';

CREATE TRIGGER trg_scenario_versions_guard
    BEFORE UPDATE OR DELETE ON scenario_versions
    FOR EACH ROW EXECUTE FUNCTION content_version_guard();
