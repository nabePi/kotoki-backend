CREATE TABLE admin_audit_logs (
    id              UUID PRIMARY KEY,
    admin_user_id   UUID NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    action          TEXT NOT NULL,
    target_type     TEXT NOT NULL,
    target_id       TEXT NOT NULL,                 -- UUID sebagai teks, atau key konten
    target_user_id  UUID NULL REFERENCES users(id) ON DELETE RESTRICT,
    request_id      TEXT NOT NULL,
    idempotency_key TEXT NULL,
    reason          TEXT NULL,
    details         JSONB NOT NULL DEFAULT '{}'::jsonb, -- before/after, angka, dsb.
    ip_address      INET NULL,
    user_agent      TEXT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT chk_admin_audit_action CHECK (action IN (
        'BALANCE_GRANT', 'BALANCE_ADJUSTMENT', 'USER_STATUS_CHANGE', 'SESSION_TRANSCRIPT_VIEW',
        'CONTENT_VERSION_CREATED', 'CONTENT_VERSION_PUBLISHED', 'CONTENT_VERSION_RETIRED',
        'CONTENT_VERSION_DELETED', 'CONTENT_STATUS_CHANGED', 'CHARACTER_UPDATED')),
    CONSTRAINT chk_admin_audit_target_type CHECK (target_type IN (
        'USER', 'SESSION', 'TOPIC', 'SCENARIO', 'CHARACTER', 'PROMPT'))
);

CREATE INDEX idx_admin_audit_created ON admin_audit_logs (id DESC);
CREATE INDEX idx_admin_audit_admin ON admin_audit_logs (admin_user_id, id DESC);
CREATE INDEX idx_admin_audit_target_user ON admin_audit_logs (target_user_id, id DESC) WHERE target_user_id IS NOT NULL;

CREATE TRIGGER trg_admin_audit_logs_append_only
    BEFORE UPDATE OR DELETE ON admin_audit_logs
    FOR EACH ROW EXECUTE FUNCTION reject_mutation();

-- TRUNCATE tidak memicu trigger baris; blokir juga di level statement.
CREATE TRIGGER trg_admin_audit_logs_no_truncate
    BEFORE TRUNCATE ON admin_audit_logs
    FOR EACH STATEMENT EXECUTE FUNCTION reject_mutation();

-- Append-only juga di level privilege (D-032): hanya dijalankan bila role aplikasi ada (lokal: dilewati).
DO $$
DECLARE r TEXT;
BEGIN
  FOREACH r IN ARRAY ARRAY['kotoki_api', 'kotoki_realtime', 'kotoki_worker'] LOOP
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = r) THEN
      EXECUTE format('REVOKE UPDATE, DELETE ON admin_audit_logs FROM %I', r);
    END IF;
  END LOOP;
END $$;
