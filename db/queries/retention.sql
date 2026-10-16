-- Retensi transcript (spec 9.9.4, D-099). Dipakai job transcript_retention lewat postgres.RetentionRepo.

-- name: PurgeEvaluationDetails :execrows
-- Langkah 1: findings dihapus, teks feedback dikosongkan; skor dan sinyal utuh (R-21). completed_at >= sessions.ended_at,
-- jadi tidak pernah lebih awal dari 180 hari setelah ended_at.
WITH doomed AS (
  SELECT id FROM evaluations
  WHERE completed_at < sqlc.arg(before)::timestamptz AND details_purged_at IS NULL
  ORDER BY completed_at LIMIT sqlc.arg(batch)::int
  FOR UPDATE SKIP LOCKED)
, del AS (DELETE FROM evaluation_findings f USING doomed d WHERE f.evaluation_id = d.id)
UPDATE evaluations e
SET summary_feedback = NULL, dimension_comments = '{}'::jsonb, strengths = '[]'::jsonb,
    focus_areas = '[]'::jsonb, recommendations = '[]'::jsonb,
    details_purged_at = now(), updated_at = now()
FROM doomed d WHERE e.id = d.id;

-- name: DeleteTurnsBefore :execrows
-- Langkah 2: (id, created_at) agar partition pruning dan index created_at lokal bekerja. Margin 2 jam dipasang pemanggil
-- pada cutoff.
DELETE FROM session_turns
WHERE (id, created_at) IN (SELECT t.id, t.created_at FROM session_turns t
                           WHERE t.created_at < sqlc.arg(cutoff)::timestamptz LIMIT sqlc.arg(batch)::int);

-- name: DeleteEventsBefore :execrows
-- Langkah 3: pola sama dengan langkah 2.
DELETE FROM session_events
WHERE (id, created_at) IN (SELECT e.id, e.created_at FROM session_events e
                           WHERE e.created_at < sqlc.arg(cutoff)::timestamptz LIMIT sqlc.arg(batch)::int);

-- name: PurgeSessionSummaries :execrows
-- Langkah 4: ringkasan sesi di-NULL dan sesi ditandai (API transcript menjawab 410 untuk sesi ini).
UPDATE sessions SET summary_text = NULL, transcript_purged_at = now(), updated_at = now()
WHERE id IN (SELECT s.id FROM sessions s
             WHERE s.ended_at < sqlc.arg(before)::timestamptz AND s.transcript_purged_at IS NULL
             LIMIT sqlc.arg(batch)::int);

-- name: DeleteExpiredRefreshTokenFamilies :execrows
-- Langkah 5a: seluruh family yang semua tokennya sudah kedaluwarsa atau direvoke sebelum cutoff, satu statement
-- (FK self-reference NO ACTION dicek di akhir statement).
DELETE FROM refresh_tokens WHERE family_id IN (
  SELECT r.family_id FROM refresh_tokens r GROUP BY r.family_id
  HAVING max(LEAST(r.expires_at, COALESCE(r.revoked_at, r.expires_at))) < sqlc.arg(cutoff)::timestamptz
  LIMIT sqlc.arg(batch)::int);

-- name: DeleteOldPasswordResetTokens :execrows
-- Langkah 5b.
DELETE FROM password_reset_tokens WHERE id IN (
  SELECT p.id FROM password_reset_tokens p WHERE p.created_at < sqlc.arg(cutoff)::timestamptz LIMIT sqlc.arg(batch)::int);
