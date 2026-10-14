CREATE TABLE prompt_configs (
    id          UUID        PRIMARY KEY,
    key         TEXT        NOT NULL,
    description TEXT        NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT ux_prompt_configs_key  UNIQUE (key),
    CONSTRAINT chk_prompt_configs_key CHECK (key IN ('CONVERSATION','OPENING','SUMMARY','EVALUATION','MEMORY_EXTRACTION')),
    CONSTRAINT chk_prompt_configs_desc CHECK (char_length(description) <= 1000)
);

CREATE TABLE prompt_versions (
    id               UUID        PRIMARY KEY,
    prompt_config_id UUID        NOT NULL,
    version          INTEGER     NOT NULL,
    status           TEXT        NOT NULL DEFAULT 'DRAFT',

    system_template  TEXT        NOT NULL,
    user_template    TEXT,                                   -- wajib SUMMARY/EVALUATION/MEMORY_EXTRACTION, NULL CONVERSATION/OPENING (validator §6.10.1)
    output_schema    JSONB,                                  -- wajib EVALUATION/MEMORY_EXTRACTION (validator §6.10.1)
    llm_params       JSONB       NOT NULL DEFAULT '{}'::jsonb,
    notes            TEXT,

    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    published_at     TIMESTAMPTZ,
    retired_at       TIMESTAMPTZ,

    CONSTRAINT fk_prompt_versions_config FOREIGN KEY (prompt_config_id)
        REFERENCES prompt_configs (id) ON DELETE RESTRICT,
    CONSTRAINT ux_prompt_versions_version UNIQUE (prompt_config_id, version),
    CONSTRAINT chk_pv_version   CHECK (version > 0),
    CONSTRAINT chk_pv_status    CHECK (status IN ('DRAFT','PUBLISHED','RETIRED')),
    CONSTRAINT chk_pv_status_ts CHECK (
           (status = 'DRAFT'     AND published_at IS NULL     AND retired_at IS NULL)
        OR (status = 'PUBLISHED' AND published_at IS NOT NULL AND retired_at IS NULL)
        OR (status = 'RETIRED'   AND published_at IS NOT NULL AND retired_at IS NOT NULL)),
    CONSTRAINT chk_pv_system_template CHECK (octet_length(system_template) BETWEEN 1 AND 16384),
    CONSTRAINT chk_pv_user_template   CHECK (user_template IS NULL OR octet_length(user_template) BETWEEN 1 AND 16384),
    CONSTRAINT chk_pv_params    CHECK (jsonb_typeof(llm_params) = 'object'
                                       AND (llm_params - ARRAY['temperature','max_output_tokens','top_p','seed']) = '{}'::jsonb),
    CONSTRAINT chk_pv_schema    CHECK (output_schema IS NULL OR jsonb_typeof(output_schema) = 'object'),
    CONSTRAINT chk_pv_notes     CHECK (notes IS NULL OR char_length(notes) <= 1000)
);

CREATE UNIQUE INDEX ux_prompt_versions_published
    ON prompt_versions (prompt_config_id) WHERE status = 'PUBLISHED';

CREATE TRIGGER trg_prompt_versions_guard
    BEFORE UPDATE OR DELETE ON prompt_versions
    FOR EACH ROW EXECUTE FUNCTION content_version_guard();
