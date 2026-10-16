-- Startup check of PROVIDER_MODE=real (spec 6.3.5, 6.13.3): ACTIVE characters whose voice slots
-- still hold PLACEHOLDER_ values. PRIMARY = characters.provider_name/model_name/voice_id,
-- FALLBACK = characters.voice.fallback. $1 = the enabled TTS vendors (UPPERCASE).

-- name: PlaceholderVoices :many
SELECT c.code::text AS character_code,
       'PRIMARY'::text AS slot,
       c.provider_name::text AS provider,
       (c.voice_id LIKE 'PLACEHOLDER\_%')::boolean AS placeholder_voice_id,
       (c.model_name LIKE 'PLACEHOLDER\_%')::boolean AS placeholder_model
FROM characters c
WHERE c.status = 'ACTIVE'
  AND c.provider_name = ANY(@vendors::text[])
  AND (c.voice_id LIKE 'PLACEHOLDER\_%' OR c.model_name LIKE 'PLACEHOLDER\_%')
UNION ALL
SELECT c.code::text,
       'FALLBACK'::text,
       (c.voice->'fallback'->>'provider_name')::text,
       (COALESCE(c.voice->'fallback'->>'voice_id', '') LIKE 'PLACEHOLDER\_%')::boolean,
       (COALESCE(c.voice->'fallback'->>'model_name', '') LIKE 'PLACEHOLDER\_%')::boolean
FROM characters c
WHERE c.status = 'ACTIVE'
  AND c.voice->'fallback'->>'provider_name' = ANY(@vendors::text[])
  AND (COALESCE(c.voice->'fallback'->>'voice_id', '') LIKE 'PLACEHOLDER\_%'
       OR COALESCE(c.voice->'fallback'->>'model_name', '') LIKE 'PLACEHOLDER\_%')
ORDER BY 1, 2;

-- name: ActiveCharactersOutsideVendors :many
SELECT c.code::text AS character_code, c.provider_name::text AS provider
FROM characters c
WHERE c.status = 'ACTIVE' AND NOT (c.provider_name = ANY(@vendors::text[]))
ORDER BY c.code;
