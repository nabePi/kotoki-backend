CREATE TABLE topics (
    id          UUID        PRIMARY KEY,
    code        TEXT        NOT NULL,
    name        TEXT        NOT NULL,
    name_ja     TEXT        NOT NULL,
    description TEXT        NOT NULL DEFAULT '',
    icon_key    TEXT,
    sort_order  INTEGER     NOT NULL DEFAULT 0,
    status      TEXT        NOT NULL DEFAULT 'ACTIVE',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT ux_topics_code        UNIQUE (code),
    CONSTRAINT chk_topics_code       CHECK (code ~ '^[A-Z][A-Z0-9_]{1,49}$'),
    CONSTRAINT chk_topics_name       CHECK (char_length(name) BETWEEN 1 AND 100),
    CONSTRAINT chk_topics_name_ja    CHECK (char_length(name_ja) BETWEEN 1 AND 100),
    CONSTRAINT chk_topics_desc       CHECK (char_length(description) <= 1000),
    CONSTRAINT chk_topics_icon_key   CHECK (icon_key IS NULL OR char_length(icon_key) BETWEEN 1 AND 100),
    CONSTRAINT chk_topics_sort_order CHECK (sort_order >= 0),
    CONSTRAINT chk_topics_status     CHECK (status IN ('ACTIVE','DISABLED','ARCHIVED'))
);

CREATE INDEX idx_topics_status_sort ON topics (status, sort_order);
