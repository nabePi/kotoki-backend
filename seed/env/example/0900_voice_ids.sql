-- seed/env/<env>/0900_voice_ids.sql (example; run with KOTOKI_ENV=example): replaces the PLACEHOLDER_ model and
-- voice ids with real ones after the Bab 8 benchmark. Only rows that still hold a placeholder are touched,
-- so running it twice, or after an admin PATCH, changes nothing.
-- The values below are the product owner's unverified examples (spec 6.13.3).
BEGIN;

UPDATE characters
SET model_name = 'Eleven v3 Conversational', voice_id = '21m00Tcm4TlvDq8ikWAM', updated_at = now()
WHERE code = 'HARUKA'
  AND voice_id LIKE 'PLACEHOLDER\_%';

-- One statement per character; change provider_name too when the benchmark picks another vendor.

COMMIT;
