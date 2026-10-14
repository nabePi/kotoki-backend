CREATE TABLE session_events (
    id          UUID NOT NULL,                 -- UUIDv7; urutan = (created_at, id)
    session_id  UUID NOT NULL REFERENCES sessions(id) ON DELETE RESTRICT,
    event_type  TEXT NOT NULL,
    actor       TEXT NOT NULL,
    from_status TEXT NULL,
    to_status   TEXT NULL,
    turn_id     UUID NULL,                     -- ID session_turns; TANPA FK (S-6), turn bisa sudah dihapus retensi (180 h < 365 h)
    payload     JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT pk_session_events PRIMARY KEY (id, created_at),

    CONSTRAINT chk_session_events_type CHECK (event_type IN (
        -- lifecycle (ditulis session.Service)
        'SESSION_RESERVED','SESSION_STARTING','SESSION_STARTED','SESSION_ENDING',
        'SESSION_COMPLETED','SESSION_CANCELLED','SESSION_ABANDONED','SESSION_FAILED',
        -- runtime (ditulis realtime lewat SessionEventWriter, Bab 7 §7.13.1)
        'OWNER_CHANGED','SESSION_DISCONNECTED','SESSION_RECONNECTED','BARGE_IN','TURN_FAILED',
        'PROVIDER_FALLBACK','PROTOCOL_VIOLATION','DIFFICULTY_ADJUSTED')),
    CONSTRAINT chk_session_events_actor CHECK (actor IN ('USER','REALTIME','SWEEPER','CLEANUP','ADMIN','SYSTEM')),
    CONSTRAINT chk_session_events_transition CHECK ((from_status IS NULL) = (to_status IS NULL)
        OR event_type = 'SESSION_RESERVED')
) PARTITION BY RANGE (created_at);

-- index lokal per partisi
CREATE INDEX idx_session_events_session ON session_events (session_id, created_at, id);
CREATE INDEX idx_session_events_created ON session_events (created_at);   -- job transcript_retention (partisi batas)

-- Partisi awal: bulan berjalan + 3 bulan ke depan (UTC). Nama: session_events_pYYYYMM.
DO $$
DECLARE m date := date_trunc('month', now() AT TIME ZONE 'UTC')::date;
BEGIN
  FOR i IN 0..3 LOOP
    EXECUTE format(
      'CREATE TABLE IF NOT EXISTS %I PARTITION OF session_events FOR VALUES FROM (%L) TO (%L)',
      'session_events_p' || to_char(m + make_interval(months => i), 'YYYYMM'),
      (m + make_interval(months => i))::timestamp AT TIME ZONE 'UTC',
      (m + make_interval(months => i + 1))::timestamp AT TIME ZONE 'UTC');
  END LOOP;
END $$;
