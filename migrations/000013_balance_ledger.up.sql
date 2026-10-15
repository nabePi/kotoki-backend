-- Fungsi dipakai juga oleh admin_audit_logs (000018)
CREATE FUNCTION reject_mutation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'table % is append-only', TG_TABLE_NAME USING ERRCODE = 'restrict_violation';
END $$;

CREATE TABLE balance_ledger (
    id                      UUID PRIMARY KEY,
    user_id                 UUID NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    account_id              UUID NOT NULL REFERENCES balance_accounts(id) ON DELETE RESTRICT,
    entry_type              TEXT NOT NULL,
    available_seconds_delta BIGINT NOT NULL,
    reserved_seconds_delta  BIGINT NOT NULL,
    available_seconds_after BIGINT NOT NULL,
    reserved_seconds_after  BIGINT NOT NULL,
    account_version         BIGINT NOT NULL,   -- balance_accounts.version setelah movement ini (urutan kanonik per account)
    session_id              UUID NULL REFERENCES sessions(id) ON DELETE RESTRICT,
    reservation_id          UUID NULL REFERENCES balance_reservations(id) ON DELETE RESTRICT,
    reference_type          TEXT NOT NULL,
    admin_user_id           UUID NULL REFERENCES users(id) ON DELETE RESTRICT,
    reason                  TEXT NULL,
    idempotency_key         TEXT NOT NULL,
    created_at              TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT ux_balance_ledger_idempotency UNIQUE (idempotency_key),
    CONSTRAINT ux_balance_ledger_account_version UNIQUE (account_id, account_version),
    CONSTRAINT chk_ledger_entry_type CHECK (entry_type IN ('GRANT', 'ADJUSTMENT', 'RESERVE', 'RELEASE', 'CONSUME')),
    CONSTRAINT chk_ledger_reference_type CHECK (reference_type IN ('SESSION', 'ADMIN', 'REGISTRATION')),
    CONSTRAINT chk_ledger_after_non_negative CHECK (available_seconds_after >= 0 AND reserved_seconds_after >= 0),
    CONSTRAINT chk_ledger_reason_len CHECK (reason IS NULL OR char_length(reason) BETWEEN 3 AND 500),
    CONSTRAINT chk_ledger_shape CHECK (
        (entry_type = 'GRANT'
            AND available_seconds_delta > 0 AND reserved_seconds_delta = 0
            AND session_id IS NULL AND reservation_id IS NULL
            AND ((reference_type = 'ADMIN' AND admin_user_id IS NOT NULL AND reason IS NOT NULL)
              OR (reference_type = 'REGISTRATION' AND admin_user_id IS NULL))
            AND idempotency_key LIKE 'grant:%')
     OR (entry_type = 'ADJUSTMENT'
            AND available_seconds_delta <> 0 AND reserved_seconds_delta = 0
            AND session_id IS NULL AND reservation_id IS NULL
            AND reference_type = 'ADMIN' AND admin_user_id IS NOT NULL AND reason IS NOT NULL
            AND idempotency_key LIKE 'adjust:%')
     OR (entry_type = 'RESERVE'
            AND reserved_seconds_delta > 0 AND available_seconds_delta = -reserved_seconds_delta
            AND session_id IS NOT NULL AND reservation_id IS NOT NULL AND reference_type = 'SESSION'
            AND idempotency_key = 'reserve:' || reservation_id::text)
     OR (entry_type = 'RELEASE'
            AND available_seconds_delta > 0 AND reserved_seconds_delta = -available_seconds_delta
            AND session_id IS NOT NULL AND reservation_id IS NOT NULL AND reference_type = 'SESSION'
            AND idempotency_key = 'release:' || reservation_id::text)
     OR (entry_type = 'CONSUME'
            AND available_seconds_delta = 0 AND reserved_seconds_delta < 0
            AND session_id IS NOT NULL AND reservation_id IS NOT NULL AND reference_type = 'SESSION'
            AND idempotency_key = 'consume:' || reservation_id::text)
    )
);

CREATE INDEX idx_balance_ledger_user ON balance_ledger (user_id, id DESC);
CREATE INDEX idx_balance_ledger_session ON balance_ledger (session_id) WHERE session_id IS NOT NULL;
CREATE INDEX idx_balance_ledger_reservation ON balance_ledger (reservation_id) WHERE reservation_id IS NOT NULL;

CREATE TRIGGER trg_balance_ledger_append_only
    BEFORE UPDATE OR DELETE ON balance_ledger
    FOR EACH ROW EXECUTE FUNCTION reject_mutation();

-- TRUNCATE tidak memicu trigger baris; blokir juga di level statement.
CREATE TRIGGER trg_balance_ledger_no_truncate
    BEFORE TRUNCATE ON balance_ledger
    FOR EACH STATEMENT EXECUTE FUNCTION reject_mutation();

-- Append-only juga di level privilege (D-032): hanya dijalankan bila role aplikasi ada (lokal: dilewati).
DO $$
DECLARE r TEXT;
BEGIN
  FOREACH r IN ARRAY ARRAY['kotoki_api', 'kotoki_realtime', 'kotoki_worker'] LOOP
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = r) THEN
      EXECUTE format('REVOKE UPDATE, DELETE ON balance_ledger FROM %I', r);
    END IF;
  END LOOP;
END $$;
