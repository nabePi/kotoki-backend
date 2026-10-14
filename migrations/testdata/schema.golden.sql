--
-- PostgreSQL database dump
--



SET statement_timeout = 0;
SET lock_timeout = 0;
SET idle_in_transaction_session_timeout = 0;
SET transaction_timeout = 0;
SET client_encoding = 'UTF8';
SET standard_conforming_strings = on;
SELECT pg_catalog.set_config('search_path', '', false);
SET check_function_bodies = false;
SET xmloption = content;
SET client_min_messages = warning;
SET row_security = off;

--
-- Name: citext; Type: EXTENSION; Schema: -; Owner: -
--

CREATE EXTENSION IF NOT EXISTS citext WITH SCHEMA public;


--
-- Name: vector; Type: EXTENSION; Schema: -; Owner: -
--

CREATE EXTENSION IF NOT EXISTS vector WITH SCHEMA public;


--
-- Name: content_version_guard(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.content_version_guard() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
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


--
-- Name: reject_mutation(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.reject_mutation() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    RAISE EXCEPTION 'table % is append-only', TG_TABLE_NAME USING ERRCODE = 'restrict_violation';
END $$;


SET default_tablespace = '';

SET default_table_access_method = heap;

--
-- Name: admin_audit_logs; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.admin_audit_logs (
    id uuid NOT NULL,
    admin_user_id uuid NOT NULL,
    action text NOT NULL,
    target_type text NOT NULL,
    target_id text NOT NULL,
    target_user_id uuid,
    request_id text NOT NULL,
    idempotency_key text,
    reason text,
    details jsonb DEFAULT '{}'::jsonb NOT NULL,
    ip_address inet,
    user_agent text,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT chk_admin_audit_action CHECK ((action = ANY (ARRAY['BALANCE_GRANT'::text, 'BALANCE_ADJUSTMENT'::text, 'USER_STATUS_CHANGE'::text, 'SESSION_TRANSCRIPT_VIEW'::text, 'CONTENT_VERSION_CREATED'::text, 'CONTENT_VERSION_PUBLISHED'::text, 'CONTENT_VERSION_RETIRED'::text, 'CONTENT_VERSION_DELETED'::text, 'CONTENT_STATUS_CHANGED'::text, 'CHARACTER_UPDATED'::text]))),
    CONSTRAINT chk_admin_audit_target_type CHECK ((target_type = ANY (ARRAY['USER'::text, 'SESSION'::text, 'TOPIC'::text, 'SCENARIO'::text, 'CHARACTER'::text, 'PROMPT'::text])))
);


--
-- Name: balance_accounts; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.balance_accounts (
    id uuid NOT NULL,
    user_id uuid NOT NULL,
    available_seconds bigint DEFAULT 0 NOT NULL,
    reserved_seconds bigint DEFAULT 0 NOT NULL,
    version bigint DEFAULT 0 NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT chk_balance_available_non_negative CHECK ((available_seconds >= 0)),
    CONSTRAINT chk_balance_reserved_non_negative CHECK ((reserved_seconds >= 0))
);


--
-- Name: balance_ledger; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.balance_ledger (
    id uuid NOT NULL,
    user_id uuid NOT NULL,
    account_id uuid NOT NULL,
    entry_type text NOT NULL,
    available_seconds_delta bigint NOT NULL,
    reserved_seconds_delta bigint NOT NULL,
    available_seconds_after bigint NOT NULL,
    reserved_seconds_after bigint NOT NULL,
    account_version bigint NOT NULL,
    session_id uuid,
    reservation_id uuid,
    reference_type text NOT NULL,
    admin_user_id uuid,
    reason text,
    idempotency_key text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT chk_ledger_after_non_negative CHECK (((available_seconds_after >= 0) AND (reserved_seconds_after >= 0))),
    CONSTRAINT chk_ledger_entry_type CHECK ((entry_type = ANY (ARRAY['GRANT'::text, 'ADJUSTMENT'::text, 'RESERVE'::text, 'RELEASE'::text, 'CONSUME'::text]))),
    CONSTRAINT chk_ledger_reason_len CHECK (((reason IS NULL) OR ((char_length(reason) >= 3) AND (char_length(reason) <= 500)))),
    CONSTRAINT chk_ledger_reference_type CHECK ((reference_type = ANY (ARRAY['SESSION'::text, 'ADMIN'::text, 'REGISTRATION'::text]))),
    CONSTRAINT chk_ledger_shape CHECK ((((entry_type = 'GRANT'::text) AND (available_seconds_delta > 0) AND (reserved_seconds_delta = 0) AND (session_id IS NULL) AND (reservation_id IS NULL) AND (((reference_type = 'ADMIN'::text) AND (admin_user_id IS NOT NULL) AND (reason IS NOT NULL)) OR ((reference_type = 'REGISTRATION'::text) AND (admin_user_id IS NULL))) AND (idempotency_key ~~ 'grant:%'::text)) OR ((entry_type = 'ADJUSTMENT'::text) AND (available_seconds_delta <> 0) AND (reserved_seconds_delta = 0) AND (session_id IS NULL) AND (reservation_id IS NULL) AND (reference_type = 'ADMIN'::text) AND (admin_user_id IS NOT NULL) AND (reason IS NOT NULL) AND (idempotency_key ~~ 'adjust:%'::text)) OR ((entry_type = 'RESERVE'::text) AND (reserved_seconds_delta > 0) AND (available_seconds_delta = (- reserved_seconds_delta)) AND (session_id IS NOT NULL) AND (reservation_id IS NOT NULL) AND (reference_type = 'SESSION'::text) AND (idempotency_key = ('reserve:'::text || (reservation_id)::text))) OR ((entry_type = 'RELEASE'::text) AND (available_seconds_delta > 0) AND (reserved_seconds_delta = (- available_seconds_delta)) AND (session_id IS NOT NULL) AND (reservation_id IS NOT NULL) AND (reference_type = 'SESSION'::text) AND (idempotency_key = ('release:'::text || (reservation_id)::text))) OR ((entry_type = 'CONSUME'::text) AND (available_seconds_delta = 0) AND (reserved_seconds_delta < 0) AND (session_id IS NOT NULL) AND (reservation_id IS NOT NULL) AND (reference_type = 'SESSION'::text) AND (idempotency_key = ('consume:'::text || (reservation_id)::text)))))
);


--
-- Name: balance_reservations; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.balance_reservations (
    id uuid NOT NULL,
    user_id uuid NOT NULL,
    account_id uuid NOT NULL,
    session_id uuid NOT NULL,
    reserved_seconds bigint NOT NULL,
    consumed_seconds bigint,
    released_seconds bigint,
    status text DEFAULT 'HELD'::text NOT NULL,
    start_deadline_at timestamp with time zone NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    settled_at timestamp with time zone,
    released_at timestamp with time zone,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT chk_reservation_deadline CHECK ((start_deadline_at > created_at)),
    CONSTRAINT chk_reservation_reserved CHECK ((reserved_seconds = ANY (ARRAY[(300)::bigint, (600)::bigint, (900)::bigint, (1800)::bigint, (2700)::bigint, (3600)::bigint]))),
    CONSTRAINT chk_reservation_state CHECK ((((status = 'HELD'::text) AND (consumed_seconds IS NULL) AND (released_seconds IS NULL) AND (settled_at IS NULL) AND (released_at IS NULL)) OR ((status = 'SETTLED'::text) AND ((consumed_seconds >= 0) AND (consumed_seconds <= reserved_seconds)) AND (released_seconds = (reserved_seconds - consumed_seconds)) AND (settled_at IS NOT NULL) AND (released_at IS NULL)) OR ((status = 'RELEASED'::text) AND (consumed_seconds = 0) AND (released_seconds = reserved_seconds) AND (released_at IS NOT NULL) AND (settled_at IS NULL)))),
    CONSTRAINT chk_reservation_status CHECK ((status = ANY (ARRAY['HELD'::text, 'SETTLED'::text, 'RELEASED'::text])))
);


--
-- Name: characters; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.characters (
    id uuid NOT NULL,
    code text NOT NULL,
    status text DEFAULT 'ACTIVE'::text NOT NULL,
    sort_order integer DEFAULT 0 NOT NULL,
    name text NOT NULL,
    avatar_key text,
    persona text NOT NULL,
    greeting_hint text,
    provider_name text NOT NULL,
    model_name text NOT NULL,
    voice_id text NOT NULL,
    voice jsonb NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT chk_characters_avatar CHECK (((avatar_key IS NULL) OR ((char_length(avatar_key) >= 1) AND (char_length(avatar_key) <= 100)))),
    CONSTRAINT chk_characters_code CHECK ((code ~ '^[A-Z][A-Z0-9_]{1,49}$'::text)),
    CONSTRAINT chk_characters_greeting CHECK (((greeting_hint IS NULL) OR ((char_length(greeting_hint) >= 1) AND (char_length(greeting_hint) <= 200)))),
    CONSTRAINT chk_characters_model CHECK (((char_length(btrim(model_name)) >= 1) AND (char_length(btrim(model_name)) <= 200))),
    CONSTRAINT chk_characters_name CHECK (((char_length(name) >= 1) AND (char_length(name) <= 50))),
    CONSTRAINT chk_characters_persona CHECK (((char_length(persona) >= 50) AND (char_length(persona) <= 3000))),
    CONSTRAINT chk_characters_provider CHECK ((provider_name = ANY (ARRAY['ELEVENLABS'::text, 'CARTESIA'::text]))),
    CONSTRAINT chk_characters_sort CHECK ((sort_order >= 0)),
    CONSTRAINT chk_characters_status CHECK ((status = ANY (ARRAY['ACTIVE'::text, 'DISABLED'::text, 'ARCHIVED'::text]))),
    CONSTRAINT chk_characters_voice_accent CHECK ((COALESCE((voice ->> 'accent'::text), ''::text) = ANY (ARRAY['KANTO'::text, 'KANSAI'::text]))),
    CONSTRAINT chk_characters_voice_fallback CHECK (((NOT (voice ? 'fallback'::text)) OR (jsonb_typeof((voice -> 'fallback'::text)) = 'object'::text))),
    CONSTRAINT chk_characters_voice_gender CHECK ((COALESCE((voice ->> 'gender'::text), ''::text) = ANY (ARRAY['FEMALE'::text, 'MALE'::text]))),
    CONSTRAINT chk_characters_voice_id CHECK (((char_length(btrim(voice_id)) >= 1) AND (char_length(btrim(voice_id)) <= 200))),
    CONSTRAINT chk_characters_voice_obj CHECK ((jsonb_typeof(voice) = 'object'::text)),
    CONSTRAINT chk_characters_voice_rate CHECK (
CASE
    WHEN (jsonb_typeof((voice -> 'speaking_rate'::text)) = 'number'::text) THEN ((((voice ->> 'speaking_rate'::text))::numeric >= 0.80) AND (((voice ->> 'speaking_rate'::text))::numeric <= 1.20))
    ELSE false
END)
);


--
-- Name: evaluation_findings; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.evaluation_findings (
    id uuid NOT NULL,
    evaluation_id uuid NOT NULL,
    session_id uuid NOT NULL,
    user_id uuid NOT NULL,
    session_turn_id uuid,
    turn_sequence_number integer,
    ordinal smallint NOT NULL,
    dimension text NOT NULL,
    finding_type text NOT NULL,
    severity text NOT NULL,
    original_text text,
    corrected_text text,
    explanation text NOT NULL,
    pattern_key text,
    confidence numeric(3,2) NOT NULL,
    source text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT chk_ef_confidence CHECK (((confidence >= (0)::numeric) AND (confidence <= (1)::numeric))),
    CONSTRAINT chk_ef_dimension CHECK ((dimension = ANY (ARRAY['GRAMMAR'::text, 'VOCABULARY'::text, 'FLUENCY'::text, 'NATURALNESS'::text, 'TASK_ACHIEVEMENT'::text]))),
    CONSTRAINT chk_ef_lengths CHECK ((((original_text IS NULL) OR (length(original_text) <= 300)) AND ((corrected_text IS NULL) OR (length(corrected_text) <= 300)) AND ((length(explanation) >= 1) AND (length(explanation) <= 600)) AND ((pattern_key IS NULL) OR (pattern_key ~ '^[a-z0-9_.]{3,64}$'::text)))),
    CONSTRAINT chk_ef_severity CHECK ((severity = ANY (ARRAY['INFO'::text, 'MINOR'::text, 'MAJOR'::text]))),
    CONSTRAINT chk_ef_source CHECK ((source = 'LLM'::text)),
    CONSTRAINT chk_ef_type_dimension CHECK (
CASE finding_type
    WHEN 'PARTICLE_ERROR'::text THEN (dimension = 'GRAMMAR'::text)
    WHEN 'CONJUGATION_ERROR'::text THEN (dimension = 'GRAMMAR'::text)
    WHEN 'TENSE_ASPECT_ERROR'::text THEN (dimension = 'GRAMMAR'::text)
    WHEN 'SENTENCE_STRUCTURE_ERROR'::text THEN (dimension = 'GRAMMAR'::text)
    WHEN 'WORD_CHOICE_ERROR'::text THEN (dimension = 'VOCABULARY'::text)
    WHEN 'NON_JAPANESE_WORD'::text THEN (dimension = 'VOCABULARY'::text)
    WHEN 'COLLOCATION_ERROR'::text THEN (dimension = 'VOCABULARY'::text)
    WHEN 'POLITENESS_MISMATCH'::text THEN (dimension = 'NATURALNESS'::text)
    WHEN 'UNNATURAL_EXPRESSION'::text THEN (dimension = 'NATURALNESS'::text)
    WHEN 'EXCESSIVE_FILLER'::text THEN (dimension = 'FLUENCY'::text)
    WHEN 'FRAGMENTED_UTTERANCE'::text THEN (dimension = 'FLUENCY'::text)
    WHEN 'OVERLY_SHORT_RESPONSE'::text THEN (dimension = 'FLUENCY'::text)
    WHEN 'OFF_TOPIC_RESPONSE'::text THEN (dimension = 'TASK_ACHIEVEMENT'::text)
    WHEN 'MISUNDERSTOOD_PROMPT'::text THEN (dimension = 'TASK_ACHIEVEMENT'::text)
    WHEN 'OBJECTIVE_NOT_MET'::text THEN (dimension = 'TASK_ACHIEVEMENT'::text)
    WHEN 'STRENGTH'::text THEN (severity = 'INFO'::text)
    ELSE false
END)
);


--
-- Name: evaluations; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.evaluations (
    id uuid NOT NULL,
    session_id uuid NOT NULL,
    user_id uuid NOT NULL,
    status text NOT NULL,
    insufficient_evidence boolean DEFAULT false NOT NULL,
    grammar_score smallint,
    vocabulary_score smallint,
    fluency_score smallint,
    naturalness_score smallint,
    task_achievement_score smallint,
    overall_score numeric(5,2),
    listening_response_signal smallint,
    interaction_complexity_signal smallint,
    dimension_confidence jsonb DEFAULT '{}'::jsonb NOT NULL,
    dimension_comments jsonb DEFAULT '{}'::jsonb NOT NULL,
    summary_feedback text,
    strengths jsonb DEFAULT '[]'::jsonb NOT NULL,
    focus_areas jsonb DEFAULT '[]'::jsonb NOT NULL,
    recommendations jsonb DEFAULT '[]'::jsonb NOT NULL,
    user_turn_count integer,
    user_speech_seconds integer,
    active_seconds integer,
    scenario_difficulty_level smallint,
    rubric_version text NOT NULL,
    prompt_version_id uuid,
    provider text,
    model text,
    llm_call_count smallint DEFAULT 0 NOT NULL,
    input_tokens integer,
    output_tokens integer,
    attempt_count integer DEFAULT 0 NOT NULL,
    failure_code text,
    last_error text,
    source_event_id uuid NOT NULL,
    details_purged_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    started_at timestamp with time zone,
    completed_at timestamp with time zone,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT chk_evaluations_attempts CHECK ((attempt_count >= 0)),
    CONSTRAINT chk_evaluations_completed_at CHECK (((status = 'COMPLETED'::text) = (completed_at IS NOT NULL))),
    CONSTRAINT chk_evaluations_completed_scores CHECK (((status <> 'COMPLETED'::text) OR insufficient_evidence OR ((grammar_score IS NOT NULL) AND (vocabulary_score IS NOT NULL) AND (fluency_score IS NOT NULL) AND (naturalness_score IS NOT NULL) AND (task_achievement_score IS NOT NULL) AND (overall_score IS NOT NULL)))),
    CONSTRAINT chk_evaluations_failed CHECK (((status <> 'FAILED'::text) OR (failure_code IS NOT NULL))),
    CONSTRAINT chk_evaluations_failure_code CHECK (((failure_code IS NULL) OR (failure_code = ANY (ARRAY['TRANSCRIPT_UNAVAILABLE'::text, 'PROVIDER_UNAVAILABLE'::text, 'INVALID_MODEL_OUTPUT'::text, 'CONFIGURATION_ERROR'::text, 'INTERNAL_ERROR'::text])))),
    CONSTRAINT chk_evaluations_insufficient CHECK (((NOT insufficient_evidence) OR ((status = 'COMPLETED'::text) AND (grammar_score IS NULL) AND (vocabulary_score IS NULL) AND (fluency_score IS NULL) AND (naturalness_score IS NULL) AND (task_achievement_score IS NULL) AND (overall_score IS NULL)))),
    CONSTRAINT chk_evaluations_scores CHECK ((((grammar_score IS NULL) OR ((grammar_score >= 0) AND (grammar_score <= 100))) AND ((vocabulary_score IS NULL) OR ((vocabulary_score >= 0) AND (vocabulary_score <= 100))) AND ((fluency_score IS NULL) OR ((fluency_score >= 0) AND (fluency_score <= 100))) AND ((naturalness_score IS NULL) OR ((naturalness_score >= 0) AND (naturalness_score <= 100))) AND ((task_achievement_score IS NULL) OR ((task_achievement_score >= 0) AND (task_achievement_score <= 100))) AND ((overall_score IS NULL) OR ((overall_score >= (0)::numeric) AND (overall_score <= (100)::numeric))) AND ((listening_response_signal IS NULL) OR ((listening_response_signal >= 0) AND (listening_response_signal <= 100))) AND ((interaction_complexity_signal IS NULL) OR ((interaction_complexity_signal >= 0) AND (interaction_complexity_signal <= 100))))),
    CONSTRAINT chk_evaluations_status CHECK ((status = ANY (ARRAY['PENDING'::text, 'RUNNING'::text, 'COMPLETED'::text, 'FAILED'::text])))
);


--
-- Name: event_outbox; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.event_outbox (
    event_id uuid NOT NULL,
    event_type text NOT NULL,
    event_version integer DEFAULT 1 NOT NULL,
    aggregate_type text NOT NULL,
    aggregate_id uuid NOT NULL,
    user_id uuid NOT NULL,
    producer text NOT NULL,
    payload jsonb NOT NULL,
    traceparent text,
    tracestate text,
    occurred_at timestamp with time zone NOT NULL,
    status text DEFAULT 'PENDING'::text NOT NULL,
    attempts integer DEFAULT 0 NOT NULL,
    available_at timestamp with time zone DEFAULT now() NOT NULL,
    last_error text,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    published_at timestamp with time zone,
    CONSTRAINT chk_event_outbox_aggregate_type CHECK ((aggregate_type = ANY (ARRAY['SESSION'::text, 'EVALUATION'::text]))),
    CONSTRAINT chk_event_outbox_attempts CHECK ((attempts >= 0)),
    CONSTRAINT chk_event_outbox_event_type CHECK ((event_type = ANY (ARRAY['session.completed'::text, 'evaluation.completed'::text, 'evaluation.failed'::text, 'memory.extracted'::text]))),
    CONSTRAINT chk_event_outbox_last_error_len CHECK (((last_error IS NULL) OR (length(last_error) <= 2000))),
    CONSTRAINT chk_event_outbox_payload_size CHECK ((octet_length((payload)::text) <= 65536)),
    CONSTRAINT chk_event_outbox_producer CHECK ((producer = ANY (ARRAY['API'::text, 'REALTIME'::text, 'WORKER'::text]))),
    CONSTRAINT chk_event_outbox_published_at CHECK (((status = 'PUBLISHED'::text) = (published_at IS NOT NULL))),
    CONSTRAINT chk_event_outbox_status CHECK ((status = ANY (ARRAY['PENDING'::text, 'PUBLISHED'::text, 'FAILED'::text]))),
    CONSTRAINT chk_event_outbox_version CHECK ((event_version >= 1))
);


--
-- Name: learner_profiles; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.learner_profiles (
    user_id uuid NOT NULL,
    nickname text,
    self_declared_level text,
    target_level text,
    learning_goal text,
    learning_goal_note text,
    preferred_speech_speed text DEFAULT 'NORMAL'::text NOT NULL,
    preferred_formality text DEFAULT 'AUTO'::text NOT NULL,
    dialect_preference text DEFAULT 'STANDARD_ONLY'::text NOT NULL,
    memory_enabled boolean DEFAULT true NOT NULL,
    onboarding_completed_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT chk_lp_dialect CHECK ((dialect_preference = ANY (ARRAY['STANDARD_ONLY'::text, 'ALLOW_DIALECT'::text]))),
    CONSTRAINT chk_lp_formality CHECK ((preferred_formality = ANY (ARRAY['AUTO'::text, 'CASUAL'::text, 'POLITE'::text]))),
    CONSTRAINT chk_lp_goal CHECK (((learning_goal IS NULL) OR (learning_goal = ANY (ARRAY['DAILY_CONVERSATION'::text, 'JOB_INTERVIEW'::text, 'BUSINESS'::text, 'TRAVEL'::text, 'EXAM_PREP'::text, 'LIVING_IN_JAPAN'::text, 'OTHER'::text])))),
    CONSTRAINT chk_lp_goal_note CHECK (((learning_goal_note IS NULL) OR (char_length(learning_goal_note) <= 300))),
    CONSTRAINT chk_lp_nickname CHECK (((nickname IS NULL) OR ((char_length(nickname) >= 1) AND (char_length(nickname) <= 30)))),
    CONSTRAINT chk_lp_self_declared CHECK (((self_declared_level IS NULL) OR (self_declared_level = ANY (ARRAY['BEGINNER'::text, 'N5'::text, 'N4'::text, 'N3'::text, 'N2'::text, 'N1'::text])))),
    CONSTRAINT chk_lp_speed CHECK ((preferred_speech_speed = ANY (ARRAY['SLOWER'::text, 'NORMAL'::text, 'FASTER'::text]))),
    CONSTRAINT chk_lp_target CHECK (((target_level IS NULL) OR (target_level = ANY (ARRAY['N5'::text, 'N4'::text, 'N3'::text, 'N2'::text, 'N1'::text]))))
);


--
-- Name: learner_skill_states; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.learner_skill_states (
    id uuid NOT NULL,
    user_id uuid NOT NULL,
    skill text NOT NULL,
    prior_level numeric(3,2) NOT NULL,
    prior_confidence numeric(4,3) NOT NULL,
    estimated_level numeric(3,2) NOT NULL,
    demonstrated_ceiling numeric(3,2),
    confidence numeric(4,3) NOT NULL,
    trend text DEFAULT 'INSUFFICIENT_DATA'::text NOT NULL,
    evidence_count integer DEFAULT 0 NOT NULL,
    last_evaluation_id uuid,
    last_evidence_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT chk_lss_ceiling CHECK (((demonstrated_ceiling IS NULL) OR ((demonstrated_ceiling >= (0)::numeric) AND (demonstrated_ceiling <= (5)::numeric)))),
    CONSTRAINT chk_lss_confidence CHECK (((confidence >= (0)::numeric) AND (confidence <= (1)::numeric))),
    CONSTRAINT chk_lss_evidence CHECK ((evidence_count >= 0)),
    CONSTRAINT chk_lss_evidence_consistency CHECK ((((evidence_count = 0) AND (last_evaluation_id IS NULL) AND (last_evidence_at IS NULL)) OR ((evidence_count > 0) AND (last_evidence_at IS NOT NULL)))),
    CONSTRAINT chk_lss_level CHECK (((estimated_level >= (0)::numeric) AND (estimated_level <= (5)::numeric))),
    CONSTRAINT chk_lss_prior CHECK ((((prior_level >= (0)::numeric) AND (prior_level <= (5)::numeric)) AND ((prior_confidence >= (0)::numeric) AND (prior_confidence <= (1)::numeric)))),
    CONSTRAINT chk_lss_skill CHECK ((skill = ANY (ARRAY['GRAMMAR'::text, 'VOCABULARY'::text, 'FLUENCY'::text, 'NATURALNESS'::text, 'LISTENING_RESPONSE'::text, 'INTERACTION_COMPLEXITY'::text]))),
    CONSTRAINT chk_lss_trend CHECK ((trend = ANY (ARRAY['INSUFFICIENT_DATA'::text, 'IMPROVING'::text, 'STABLE'::text, 'DECLINING'::text])))
);


--
-- Name: memories; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.memories (
    id uuid NOT NULL,
    user_id uuid NOT NULL,
    category text NOT NULL,
    memory_key text NOT NULL,
    content text NOT NULL,
    display_text text NOT NULL,
    evidence_type text NOT NULL,
    confidence numeric(4,3) NOT NULL,
    importance numeric(4,3) NOT NULL,
    observation_count integer DEFAULT 1 NOT NULL,
    source_session_id uuid,
    source_turn_ids uuid[] DEFAULT '{}'::uuid[] NOT NULL,
    extraction_prompt_version_id uuid,
    embedding public.vector(1536),
    embedding_model text,
    status text DEFAULT 'ACTIVE'::text NOT NULL,
    superseded_by uuid,
    first_observed_at timestamp with time zone NOT NULL,
    last_observed_at timestamp with time zone NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT chk_memories_category CHECK ((category = ANY (ARRAY['PROFILE_FACT'::text, 'PREFERENCE'::text, 'LEARNING_PATTERN'::text, 'EPISODIC'::text]))),
    CONSTRAINT chk_memories_confidence CHECK (((confidence >= (0)::numeric) AND (confidence <= (1)::numeric))),
    CONSTRAINT chk_memories_content CHECK ((((status = 'DELETED'::text) AND (content = ''::text) AND (display_text = ''::text) AND (embedding IS NULL) AND (evidence_type = 'USER_DELETED'::text)) OR ((status <> 'DELETED'::text) AND (evidence_type <> 'USER_DELETED'::text) AND ((char_length(content) >= 1) AND (char_length(content) <= 200)) AND ((char_length(display_text) >= 1) AND (char_length(display_text) <= 400))))),
    CONSTRAINT chk_memories_evidence CHECK ((evidence_type = ANY (ARRAY['EXPLICIT_USER_STATEMENT'::text, 'USER_CONFIRMED'::text, 'INFERRED'::text, 'USER_DELETED'::text]))),
    CONSTRAINT chk_memories_importance CHECK (((importance >= (0)::numeric) AND (importance <= (1)::numeric))),
    CONSTRAINT chk_memories_key_fmt CHECK (((memory_key ~ '^[a-z0-9_]+(:[a-z0-9_-]+){0,2}$'::text) AND (char_length(memory_key) <= 100))),
    CONSTRAINT chk_memories_obs CHECK ((observation_count >= 1)),
    CONSTRAINT chk_memories_observed CHECK ((last_observed_at >= first_observed_at)),
    CONSTRAINT chk_memories_status CHECK ((status = ANY (ARRAY['ACTIVE'::text, 'SUPERSEDED'::text, 'DELETED'::text]))),
    CONSTRAINT chk_memories_superseded_by CHECK (((status = 'SUPERSEDED'::text) OR (superseded_by IS NULL)))
);


--
-- Name: password_reset_tokens; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.password_reset_tokens (
    id uuid NOT NULL,
    user_id uuid NOT NULL,
    token_hash bytea NOT NULL,
    requested_ip inet,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    used_at timestamp with time zone,
    invalidated_at timestamp with time zone,
    CONSTRAINT chk_reset_expiry CHECK ((expires_at > created_at)),
    CONSTRAINT chk_reset_hash_len CHECK ((octet_length(token_hash) = 32))
);


--
-- Name: processed_events; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.processed_events (
    consumer text NOT NULL,
    event_id uuid NOT NULL,
    event_type text NOT NULL,
    processed_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT chk_processed_events_consumer CHECK ((consumer = ANY (ARRAY['evaluation.session_completed'::text, 'memory.session_completed'::text, 'summary.session_completed'::text, 'learner.evaluation_completed'::text])))
);


--
-- Name: proficiency_estimates; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.proficiency_estimates (
    id uuid NOT NULL,
    user_id uuid NOT NULL,
    skill text NOT NULL,
    source_evaluation_id uuid NOT NULL,
    session_id uuid NOT NULL,
    algorithm_version smallint NOT NULL,
    raw_score numeric(5,2) NOT NULL,
    effective_difficulty smallint,
    evidence_level numeric(3,2) NOT NULL,
    evidence_confidence numeric(4,3) NOT NULL,
    previous_level numeric(3,2) NOT NULL,
    estimated_level numeric(3,2) NOT NULL,
    confidence numeric(4,3) NOT NULL,
    evidence_count integer NOT NULL,
    trend text NOT NULL,
    assessed_at timestamp with time zone NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT chk_pe_conf CHECK ((((evidence_confidence >= (0)::numeric) AND (evidence_confidence <= (1)::numeric)) AND ((confidence >= (0)::numeric) AND (confidence <= (1)::numeric)))),
    CONSTRAINT chk_pe_count CHECK ((evidence_count >= 1)),
    CONSTRAINT chk_pe_deff CHECK (((effective_difficulty IS NULL) OR ((effective_difficulty >= 1) AND (effective_difficulty <= 5)))),
    CONSTRAINT chk_pe_levels CHECK ((((evidence_level >= (0)::numeric) AND (evidence_level <= (5)::numeric)) AND ((previous_level >= (0)::numeric) AND (previous_level <= (5)::numeric)) AND ((estimated_level >= (0)::numeric) AND (estimated_level <= (5)::numeric)))),
    CONSTRAINT chk_pe_raw CHECK (((raw_score >= (0)::numeric) AND (raw_score <= (100)::numeric))),
    CONSTRAINT chk_pe_skill CHECK ((skill = ANY (ARRAY['GRAMMAR'::text, 'VOCABULARY'::text, 'FLUENCY'::text, 'NATURALNESS'::text, 'LISTENING_RESPONSE'::text, 'INTERACTION_COMPLEXITY'::text]))),
    CONSTRAINT chk_pe_trend CHECK ((trend = ANY (ARRAY['INSUFFICIENT_DATA'::text, 'IMPROVING'::text, 'STABLE'::text, 'DECLINING'::text])))
);


--
-- Name: prompt_configs; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.prompt_configs (
    id uuid NOT NULL,
    key text NOT NULL,
    description text DEFAULT ''::text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT chk_prompt_configs_desc CHECK ((char_length(description) <= 1000)),
    CONSTRAINT chk_prompt_configs_key CHECK ((key = ANY (ARRAY['CONVERSATION'::text, 'OPENING'::text, 'SUMMARY'::text, 'EVALUATION'::text, 'MEMORY_EXTRACTION'::text])))
);


--
-- Name: prompt_versions; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.prompt_versions (
    id uuid NOT NULL,
    prompt_config_id uuid NOT NULL,
    version integer NOT NULL,
    status text DEFAULT 'DRAFT'::text NOT NULL,
    system_template text NOT NULL,
    user_template text,
    output_schema jsonb,
    llm_params jsonb DEFAULT '{}'::jsonb NOT NULL,
    notes text,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    published_at timestamp with time zone,
    retired_at timestamp with time zone,
    CONSTRAINT chk_pv_notes CHECK (((notes IS NULL) OR (char_length(notes) <= 1000))),
    CONSTRAINT chk_pv_params CHECK (((jsonb_typeof(llm_params) = 'object'::text) AND ((llm_params - ARRAY['temperature'::text, 'max_output_tokens'::text, 'top_p'::text, 'seed'::text]) = '{}'::jsonb))),
    CONSTRAINT chk_pv_schema CHECK (((output_schema IS NULL) OR (jsonb_typeof(output_schema) = 'object'::text))),
    CONSTRAINT chk_pv_status CHECK ((status = ANY (ARRAY['DRAFT'::text, 'PUBLISHED'::text, 'RETIRED'::text]))),
    CONSTRAINT chk_pv_status_ts CHECK ((((status = 'DRAFT'::text) AND (published_at IS NULL) AND (retired_at IS NULL)) OR ((status = 'PUBLISHED'::text) AND (published_at IS NOT NULL) AND (retired_at IS NULL)) OR ((status = 'RETIRED'::text) AND (published_at IS NOT NULL) AND (retired_at IS NOT NULL)))),
    CONSTRAINT chk_pv_system_template CHECK (((octet_length(system_template) >= 1) AND (octet_length(system_template) <= 16384))),
    CONSTRAINT chk_pv_user_template CHECK (((user_template IS NULL) OR ((octet_length(user_template) >= 1) AND (octet_length(user_template) <= 16384)))),
    CONSTRAINT chk_pv_version CHECK ((version > 0))
);


--
-- Name: refresh_tokens; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.refresh_tokens (
    id uuid NOT NULL,
    user_id uuid NOT NULL,
    family_id uuid NOT NULL,
    parent_id uuid,
    token_hash bytea NOT NULL,
    user_agent text,
    ip_address inet,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    revoked_at timestamp with time zone,
    revoke_reason text,
    replaced_by_id uuid,
    CONSTRAINT chk_refresh_hash_len CHECK ((octet_length(token_hash) = 32)),
    CONSTRAINT chk_refresh_revoke CHECK (((revoked_at IS NULL) = (revoke_reason IS NULL))),
    CONSTRAINT chk_refresh_revoke_reason CHECK (((revoke_reason IS NULL) OR (revoke_reason = ANY (ARRAY['ROTATED'::text, 'LOGOUT'::text, 'REUSE_DETECTED'::text, 'PASSWORD_RESET'::text, 'USER_STATUS_CHANGED'::text]))))
);


--
-- Name: scenario_versions; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.scenario_versions (
    id uuid NOT NULL,
    scenario_id uuid NOT NULL,
    version integer NOT NULL,
    status text DEFAULT 'DRAFT'::text NOT NULL,
    difficulty_level smallint NOT NULL,
    recommended_level_min numeric(2,1) NOT NULL,
    recommended_level_max numeric(2,1) NOT NULL,
    situation text NOT NULL,
    ai_role text NOT NULL,
    user_role text NOT NULL,
    goal text NOT NULL,
    tasks jsonb DEFAULT '[]'::jsonb NOT NULL,
    constraints jsonb DEFAULT '[]'::jsonb NOT NULL,
    grammar_targets jsonb DEFAULT '[]'::jsonb NOT NULL,
    vocab_hints jsonb DEFAULT '[]'::jsonb NOT NULL,
    evaluation_focus text[] DEFAULT '{}'::text[] NOT NULL,
    formality text DEFAULT 'POLITE'::text NOT NULL,
    opening_line text NOT NULL,
    visual_prompt jsonb NOT NULL,
    notes text,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    published_at timestamp with time zone,
    retired_at timestamp with time zone,
    CONSTRAINT chk_scenario_versions_ai_role CHECK (((char_length(ai_role) >= 1) AND (char_length(ai_role) <= 300))),
    CONSTRAINT chk_scenario_versions_constraints CHECK (((jsonb_typeof(constraints) = 'array'::text) AND (jsonb_array_length(constraints) <= 8))),
    CONSTRAINT chk_scenario_versions_difficulty CHECK (((difficulty_level >= 1) AND (difficulty_level <= 5))),
    CONSTRAINT chk_scenario_versions_evalfocus CHECK (((cardinality(evaluation_focus) <= 3) AND (evaluation_focus <@ ARRAY['GRAMMAR'::text, 'VOCABULARY'::text, 'FLUENCY'::text, 'NATURALNESS'::text, 'LISTENING_RESPONSE'::text, 'INTERACTION_COMPLEXITY'::text]))),
    CONSTRAINT chk_scenario_versions_formality CHECK ((formality = ANY (ARRAY['CASUAL'::text, 'POLITE'::text, 'KEIGO'::text]))),
    CONSTRAINT chk_scenario_versions_goal CHECK (((char_length(goal) >= 1) AND (char_length(goal) <= 500))),
    CONSTRAINT chk_scenario_versions_grammar CHECK (((jsonb_typeof(grammar_targets) = 'array'::text) AND (jsonb_array_length(grammar_targets) <= 8))),
    CONSTRAINT chk_scenario_versions_levels CHECK ((((recommended_level_min >= 0.0) AND (recommended_level_min <= 5.0)) AND ((recommended_level_max >= 0.0) AND (recommended_level_max <= 5.0)) AND (recommended_level_min <= recommended_level_max))),
    CONSTRAINT chk_scenario_versions_notes CHECK (((notes IS NULL) OR (char_length(notes) <= 1000))),
    CONSTRAINT chk_scenario_versions_opening CHECK (((char_length(opening_line) >= 1) AND (char_length(opening_line) <= 200))),
    CONSTRAINT chk_scenario_versions_situation CHECK (((char_length(situation) >= 1) AND (char_length(situation) <= 2000))),
    CONSTRAINT chk_scenario_versions_status CHECK ((status = ANY (ARRAY['DRAFT'::text, 'PUBLISHED'::text, 'RETIRED'::text]))),
    CONSTRAINT chk_scenario_versions_status_ts CHECK ((((status = 'DRAFT'::text) AND (published_at IS NULL) AND (retired_at IS NULL)) OR ((status = 'PUBLISHED'::text) AND (published_at IS NOT NULL) AND (retired_at IS NULL)) OR ((status = 'RETIRED'::text) AND (published_at IS NOT NULL) AND (retired_at IS NOT NULL)))),
    CONSTRAINT chk_scenario_versions_tasks CHECK (((jsonb_typeof(tasks) = 'array'::text) AND (jsonb_array_length(tasks) <= 6))),
    CONSTRAINT chk_scenario_versions_user_role CHECK (((char_length(user_role) >= 1) AND (char_length(user_role) <= 300))),
    CONSTRAINT chk_scenario_versions_version CHECK ((version > 0)),
    CONSTRAINT chk_scenario_versions_visual CHECK ((jsonb_typeof(visual_prompt) = 'object'::text)),
    CONSTRAINT chk_scenario_versions_vocab CHECK (((jsonb_typeof(vocab_hints) = 'array'::text) AND (jsonb_array_length(vocab_hints) <= 20)))
);


--
-- Name: scenarios; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.scenarios (
    id uuid NOT NULL,
    topic_id uuid NOT NULL,
    code text NOT NULL,
    name text NOT NULL,
    name_ja text NOT NULL,
    description text DEFAULT ''::text NOT NULL,
    sort_order integer DEFAULT 0 NOT NULL,
    status text DEFAULT 'ACTIVE'::text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT chk_scenarios_code CHECK ((code ~ '^[A-Z][A-Z0-9_]{1,79}$'::text)),
    CONSTRAINT chk_scenarios_desc CHECK ((char_length(description) <= 1000)),
    CONSTRAINT chk_scenarios_name CHECK (((char_length(name) >= 1) AND (char_length(name) <= 150))),
    CONSTRAINT chk_scenarios_name_ja CHECK (((char_length(name_ja) >= 1) AND (char_length(name_ja) <= 150))),
    CONSTRAINT chk_scenarios_sort_order CHECK ((sort_order >= 0)),
    CONSTRAINT chk_scenarios_status CHECK ((status = ANY (ARRAY['ACTIVE'::text, 'DISABLED'::text, 'ARCHIVED'::text])))
);


--
-- Name: schema_migrations; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.schema_migrations (
    version bigint NOT NULL,
    dirty boolean NOT NULL
);


--
-- Name: session_events; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.session_events (
    id uuid NOT NULL,
    session_id uuid NOT NULL,
    event_type text NOT NULL,
    actor text NOT NULL,
    from_status text,
    to_status text,
    turn_id uuid,
    payload jsonb DEFAULT '{}'::jsonb NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT chk_session_events_actor CHECK ((actor = ANY (ARRAY['USER'::text, 'REALTIME'::text, 'SWEEPER'::text, 'CLEANUP'::text, 'ADMIN'::text, 'SYSTEM'::text]))),
    CONSTRAINT chk_session_events_transition CHECK ((((from_status IS NULL) = (to_status IS NULL)) OR (event_type = 'SESSION_RESERVED'::text))),
    CONSTRAINT chk_session_events_type CHECK ((event_type = ANY (ARRAY['SESSION_RESERVED'::text, 'SESSION_STARTING'::text, 'SESSION_STARTED'::text, 'SESSION_ENDING'::text, 'SESSION_COMPLETED'::text, 'SESSION_CANCELLED'::text, 'SESSION_ABANDONED'::text, 'SESSION_FAILED'::text, 'OWNER_CHANGED'::text, 'SESSION_DISCONNECTED'::text, 'SESSION_RECONNECTED'::text, 'BARGE_IN'::text, 'TURN_FAILED'::text, 'PROVIDER_FALLBACK'::text, 'PROTOCOL_VIOLATION'::text, 'DIFFICULTY_ADJUSTED'::text])))
)
PARTITION BY RANGE (created_at);


--
-- Name: session_turns; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.session_turns (
    id uuid NOT NULL,
    session_id uuid NOT NULL,
    sequence_number integer NOT NULL,
    status text,
    user_text text,
    user_started_at timestamp with time zone,
    user_ended_at timestamp with time zone,
    assistant_text text,
    assistant_spoken_text text,
    generation integer NOT NULL,
    stt_provider text,
    llm_provider text,
    llm_model text,
    tts_provider text,
    tts_model text,
    tts_voice_id text,
    used_fallback boolean DEFAULT false NOT NULL,
    turn_end_to_first_audio_ms integer,
    stt_final_ms integer,
    llm_first_token_ms integer,
    tts_first_audio_ms integer,
    input_tokens integer,
    output_tokens integer,
    tts_characters integer,
    stt_audio_ms integer,
    error_code text,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    completed_at timestamp with time zone,
    CONSTRAINT chk_turn_completed CHECK (((status IS NULL) = (completed_at IS NULL))),
    CONSTRAINT chk_turn_generation CHECK ((generation >= 0)),
    CONSTRAINT chk_turn_metrics CHECK (((COALESCE(turn_end_to_first_audio_ms, 0) >= 0) AND (COALESCE(stt_final_ms, 0) >= 0) AND (COALESCE(llm_first_token_ms, 0) >= 0) AND (COALESCE(tts_first_audio_ms, 0) >= 0) AND (COALESCE(input_tokens, 0) >= 0) AND (COALESCE(output_tokens, 0) >= 0) AND (COALESCE(tts_characters, 0) >= 0) AND (COALESCE(stt_audio_ms, 0) >= 0))),
    CONSTRAINT chk_turn_opening CHECK (((sequence_number > 0) OR (user_text IS NULL))),
    CONSTRAINT chk_turn_provider_upper CHECK (((COALESCE(stt_provider, 'X'::text) ~ '^[A-Z][A-Z0-9_]*$'::text) AND (COALESCE(llm_provider, 'X'::text) ~ '^[A-Z][A-Z0-9_]*$'::text) AND (COALESCE(tts_provider, 'X'::text) ~ '^[A-Z][A-Z0-9_]*$'::text))),
    CONSTRAINT chk_turn_seq CHECK ((sequence_number >= 0)),
    CONSTRAINT chk_turn_status CHECK (((status IS NULL) OR (status = ANY (ARRAY['COMPLETED'::text, 'INTERRUPTED'::text, 'FAILED'::text, 'DISCARDED'::text]))))
)
PARTITION BY RANGE (created_at);


--
-- Name: sessions; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.sessions (
    id uuid NOT NULL,
    user_id uuid NOT NULL,
    status text NOT NULL,
    end_reason text,
    conversation_start_mode text NOT NULL,
    duration_seconds integer NOT NULL,
    topic_id uuid NOT NULL,
    scenario_id uuid NOT NULL,
    scenario_version_id uuid NOT NULL,
    character_id uuid NOT NULL,
    prompt_version_id uuid NOT NULL,
    opening_prompt_version_id uuid,
    config_snapshot jsonb NOT NULL,
    adaptive_difficulty jsonb NOT NULL,
    runtime_fence bigint DEFAULT 0 NOT NULL,
    reserved_seconds integer NOT NULL,
    charged_seconds integer DEFAULT 0 NOT NULL,
    disconnected_seconds integer DEFAULT 0 NOT NULL,
    failure_detail text,
    summary_text text,
    transcript_purged_at timestamp with time zone,
    create_idempotency_key text NOT NULL,
    create_request_hash bytea NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    starting_at timestamp with time zone,
    started_at timestamp with time zone,
    expires_at timestamp with time zone,
    ending_at timestamp with time zone,
    ended_at timestamp with time zone,
    handoff_at timestamp with time zone,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    version bigint DEFAULT 1 NOT NULL,
    CONSTRAINT chk_sessions_adaptive CHECK ((jsonb_typeof(adaptive_difficulty) = 'object'::text)),
    CONSTRAINT chk_sessions_charged CHECK (((charged_seconds >= 0) AND (charged_seconds <= reserved_seconds))),
    CONSTRAINT chk_sessions_config_snapshot CHECK ((jsonb_typeof(config_snapshot) = 'object'::text)),
    CONSTRAINT chk_sessions_disconnected CHECK (((disconnected_seconds >= 0) AND (disconnected_seconds <= duration_seconds))),
    CONSTRAINT chk_sessions_duration CHECK ((duration_seconds = ANY (ARRAY[300, 600, 900, 1800, 2700, 3600]))),
    CONSTRAINT chk_sessions_end_reason CHECK (((end_reason IS NULL) OR (end_reason = ANY (ARRAY['USER_ENDED'::text, 'DURATION_ELAPSED'::text, 'RECONNECT_TIMEOUT'::text, 'PROVIDER_FAILURE'::text, 'SERVER_ERROR'::text, 'USER_CANCELLED'::text, 'NOT_STARTED'::text])))),
    CONSTRAINT chk_sessions_expires CHECK ((((started_at IS NULL) AND (expires_at IS NULL)) OR (expires_at = (started_at + make_interval(secs => (duration_seconds)::double precision))))),
    CONSTRAINT chk_sessions_failed_unstarted_free CHECK ((NOT ((status = 'FAILED'::text) AND (started_at IS NULL) AND (charged_seconds <> 0)))),
    CONSTRAINT chk_sessions_handoff CHECK (((handoff_at IS NULL) OR ((started_at IS NOT NULL) AND (handoff_at >= started_at)))),
    CONSTRAINT chk_sessions_idem_key CHECK ((create_idempotency_key ~ '^[A-Za-z0-9_-]{8,128}$'::text)),
    CONSTRAINT chk_sessions_opening_prompt CHECK (((opening_prompt_version_id IS NULL) OR ((conversation_start_mode = 'AI_FIRST'::text) AND (status <> 'RESERVED'::text)))),
    CONSTRAINT chk_sessions_reserved CHECK ((reserved_seconds = duration_seconds)),
    CONSTRAINT chk_sessions_runtime_fence CHECK ((runtime_fence >= 0)),
    CONSTRAINT chk_sessions_start_mode CHECK ((conversation_start_mode = ANY (ARRAY['USER_FIRST'::text, 'AI_FIRST'::text]))),
    CONSTRAINT chk_sessions_state CHECK ((((status = 'RESERVED'::text) AND (end_reason IS NULL) AND (starting_at IS NULL) AND (started_at IS NULL) AND (ending_at IS NULL) AND (ended_at IS NULL)) OR ((status = 'STARTING'::text) AND (end_reason IS NULL) AND (starting_at IS NOT NULL) AND (started_at IS NULL) AND (ending_at IS NULL) AND (ended_at IS NULL)) OR ((status = 'ACTIVE'::text) AND (end_reason IS NULL) AND (started_at IS NOT NULL) AND (ending_at IS NULL) AND (ended_at IS NULL)) OR ((status = 'ENDING'::text) AND (end_reason = ANY (ARRAY['USER_ENDED'::text, 'DURATION_ELAPSED'::text, 'RECONNECT_TIMEOUT'::text, 'PROVIDER_FAILURE'::text, 'SERVER_ERROR'::text])) AND (started_at IS NOT NULL) AND (ending_at IS NOT NULL) AND (ended_at IS NULL)) OR ((status = 'COMPLETED'::text) AND (end_reason = ANY (ARRAY['USER_ENDED'::text, 'DURATION_ELAPSED'::text, 'RECONNECT_TIMEOUT'::text])) AND (started_at IS NOT NULL) AND (ending_at IS NOT NULL) AND (ended_at IS NOT NULL)) OR ((status = 'FAILED'::text) AND (end_reason = ANY (ARRAY['PROVIDER_FAILURE'::text, 'SERVER_ERROR'::text])) AND (ended_at IS NOT NULL) AND (starting_at IS NOT NULL)) OR ((status = 'CANCELLED'::text) AND (end_reason = 'USER_CANCELLED'::text) AND (started_at IS NULL) AND (ended_at IS NOT NULL) AND (charged_seconds = 0)) OR ((status = 'ABANDONED'::text) AND (end_reason = 'NOT_STARTED'::text) AND (started_at IS NULL) AND (ended_at IS NOT NULL) AND (charged_seconds = 0)))),
    CONSTRAINT chk_sessions_status CHECK ((status = ANY (ARRAY['RESERVED'::text, 'STARTING'::text, 'ACTIVE'::text, 'ENDING'::text, 'COMPLETED'::text, 'CANCELLED'::text, 'ABANDONED'::text, 'FAILED'::text])))
);


--
-- Name: topics; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.topics (
    id uuid NOT NULL,
    code text NOT NULL,
    name text NOT NULL,
    name_ja text NOT NULL,
    description text DEFAULT ''::text NOT NULL,
    icon_key text,
    sort_order integer DEFAULT 0 NOT NULL,
    status text DEFAULT 'ACTIVE'::text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT chk_topics_code CHECK ((code ~ '^[A-Z][A-Z0-9_]{1,49}$'::text)),
    CONSTRAINT chk_topics_desc CHECK ((char_length(description) <= 1000)),
    CONSTRAINT chk_topics_icon_key CHECK (((icon_key IS NULL) OR ((char_length(icon_key) >= 1) AND (char_length(icon_key) <= 100)))),
    CONSTRAINT chk_topics_name CHECK (((char_length(name) >= 1) AND (char_length(name) <= 100))),
    CONSTRAINT chk_topics_name_ja CHECK (((char_length(name_ja) >= 1) AND (char_length(name_ja) <= 100))),
    CONSTRAINT chk_topics_sort_order CHECK ((sort_order >= 0)),
    CONSTRAINT chk_topics_status CHECK ((status = ANY (ARRAY['ACTIVE'::text, 'DISABLED'::text, 'ARCHIVED'::text])))
);


--
-- Name: users; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.users (
    id uuid NOT NULL,
    email public.citext NOT NULL,
    phone_number text,
    full_name text NOT NULL,
    password_hash text NOT NULL,
    role text DEFAULT 'USER'::text NOT NULL,
    status text DEFAULT 'ACTIVE'::text NOT NULL,
    password_changed_at timestamp with time zone DEFAULT now() NOT NULL,
    last_login_at timestamp with time zone,
    terms_version text NOT NULL,
    terms_accepted_at timestamp with time zone NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    deleted_at timestamp with time zone,
    purged_at timestamp with time zone,
    CONSTRAINT chk_users_deleted_at CHECK (((status = 'DELETED'::text) = (deleted_at IS NOT NULL))),
    CONSTRAINT chk_users_email_len CHECK (((char_length((email)::text) >= 3) AND (char_length((email)::text) <= 254))),
    CONSTRAINT chk_users_full_name CHECK (((char_length(btrim(full_name)) >= 1) AND (char_length(btrim(full_name)) <= 100))),
    CONSTRAINT chk_users_phone_fmt CHECK (((phone_number IS NULL) OR (phone_number ~ '^\+[1-9][0-9]{7,14}$'::text))),
    CONSTRAINT chk_users_purged_at CHECK (((purged_at IS NULL) OR ((status = 'DELETED'::text) AND (purged_at >= deleted_at)))),
    CONSTRAINT chk_users_role CHECK ((role = ANY (ARRAY['USER'::text, 'ADMIN'::text]))),
    CONSTRAINT chk_users_status CHECK ((status = ANY (ARRAY['ACTIVE'::text, 'SUSPENDED'::text, 'DELETED'::text]))),
    CONSTRAINT chk_users_terms_version CHECK ((terms_version ~ '^[A-Za-z0-9._-]{1,32}$'::text))
);


--
-- Name: admin_audit_logs admin_audit_logs_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.admin_audit_logs
    ADD CONSTRAINT admin_audit_logs_pkey PRIMARY KEY (id);


--
-- Name: balance_accounts balance_accounts_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.balance_accounts
    ADD CONSTRAINT balance_accounts_pkey PRIMARY KEY (id);


--
-- Name: balance_ledger balance_ledger_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.balance_ledger
    ADD CONSTRAINT balance_ledger_pkey PRIMARY KEY (id);


--
-- Name: balance_reservations balance_reservations_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.balance_reservations
    ADD CONSTRAINT balance_reservations_pkey PRIMARY KEY (id);


--
-- Name: characters characters_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.characters
    ADD CONSTRAINT characters_pkey PRIMARY KEY (id);


--
-- Name: evaluation_findings evaluation_findings_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.evaluation_findings
    ADD CONSTRAINT evaluation_findings_pkey PRIMARY KEY (id);


--
-- Name: evaluations evaluations_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.evaluations
    ADD CONSTRAINT evaluations_pkey PRIMARY KEY (id);


--
-- Name: event_outbox event_outbox_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.event_outbox
    ADD CONSTRAINT event_outbox_pkey PRIMARY KEY (event_id);


--
-- Name: learner_profiles learner_profiles_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.learner_profiles
    ADD CONSTRAINT learner_profiles_pkey PRIMARY KEY (user_id);


--
-- Name: learner_skill_states learner_skill_states_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.learner_skill_states
    ADD CONSTRAINT learner_skill_states_pkey PRIMARY KEY (id);


--
-- Name: memories memories_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.memories
    ADD CONSTRAINT memories_pkey PRIMARY KEY (id);


--
-- Name: password_reset_tokens password_reset_tokens_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.password_reset_tokens
    ADD CONSTRAINT password_reset_tokens_pkey PRIMARY KEY (id);


--
-- Name: session_events pk_session_events; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.session_events
    ADD CONSTRAINT pk_session_events PRIMARY KEY (id, created_at);


--
-- Name: session_turns pk_session_turns; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.session_turns
    ADD CONSTRAINT pk_session_turns PRIMARY KEY (id, created_at);


--
-- Name: processed_events processed_events_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.processed_events
    ADD CONSTRAINT processed_events_pkey PRIMARY KEY (consumer, event_id);


--
-- Name: proficiency_estimates proficiency_estimates_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.proficiency_estimates
    ADD CONSTRAINT proficiency_estimates_pkey PRIMARY KEY (id);


--
-- Name: prompt_configs prompt_configs_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.prompt_configs
    ADD CONSTRAINT prompt_configs_pkey PRIMARY KEY (id);


--
-- Name: prompt_versions prompt_versions_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.prompt_versions
    ADD CONSTRAINT prompt_versions_pkey PRIMARY KEY (id);


--
-- Name: refresh_tokens refresh_tokens_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.refresh_tokens
    ADD CONSTRAINT refresh_tokens_pkey PRIMARY KEY (id);


--
-- Name: scenario_versions scenario_versions_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.scenario_versions
    ADD CONSTRAINT scenario_versions_pkey PRIMARY KEY (id);


--
-- Name: scenarios scenarios_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.scenarios
    ADD CONSTRAINT scenarios_pkey PRIMARY KEY (id);


--
-- Name: schema_migrations schema_migrations_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.schema_migrations
    ADD CONSTRAINT schema_migrations_pkey PRIMARY KEY (version);


--
-- Name: session_turns ux_session_turns_seq; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.session_turns
    ADD CONSTRAINT ux_session_turns_seq UNIQUE (session_id, sequence_number, created_at);


--
-- Name: sessions sessions_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.sessions
    ADD CONSTRAINT sessions_pkey PRIMARY KEY (id);


--
-- Name: topics topics_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.topics
    ADD CONSTRAINT topics_pkey PRIMARY KEY (id);


--
-- Name: evaluation_findings uq_evaluation_findings_ordinal; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.evaluation_findings
    ADD CONSTRAINT uq_evaluation_findings_ordinal UNIQUE (evaluation_id, ordinal);


--
-- Name: evaluations uq_evaluations_session; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.evaluations
    ADD CONSTRAINT uq_evaluations_session UNIQUE (session_id);


--
-- Name: users users_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.users
    ADD CONSTRAINT users_pkey PRIMARY KEY (id);


--
-- Name: balance_accounts ux_balance_accounts_user; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.balance_accounts
    ADD CONSTRAINT ux_balance_accounts_user UNIQUE (user_id);


--
-- Name: balance_ledger ux_balance_ledger_account_version; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.balance_ledger
    ADD CONSTRAINT ux_balance_ledger_account_version UNIQUE (account_id, account_version);


--
-- Name: balance_ledger ux_balance_ledger_idempotency; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.balance_ledger
    ADD CONSTRAINT ux_balance_ledger_idempotency UNIQUE (idempotency_key);


--
-- Name: balance_reservations ux_balance_reservations_session; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.balance_reservations
    ADD CONSTRAINT ux_balance_reservations_session UNIQUE (session_id);


--
-- Name: characters ux_characters_code; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.characters
    ADD CONSTRAINT ux_characters_code UNIQUE (code);


--
-- Name: learner_skill_states ux_learner_skill_states_user_skill; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.learner_skill_states
    ADD CONSTRAINT ux_learner_skill_states_user_skill UNIQUE (user_id, skill);


--
-- Name: proficiency_estimates ux_proficiency_estimates_eval_skill; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.proficiency_estimates
    ADD CONSTRAINT ux_proficiency_estimates_eval_skill UNIQUE (source_evaluation_id, skill);


--
-- Name: prompt_configs ux_prompt_configs_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.prompt_configs
    ADD CONSTRAINT ux_prompt_configs_key UNIQUE (key);


--
-- Name: prompt_versions ux_prompt_versions_version; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.prompt_versions
    ADD CONSTRAINT ux_prompt_versions_version UNIQUE (prompt_config_id, version);


--
-- Name: scenario_versions ux_scenario_versions_version; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.scenario_versions
    ADD CONSTRAINT ux_scenario_versions_version UNIQUE (scenario_id, version);


--
-- Name: scenarios ux_scenarios_code; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.scenarios
    ADD CONSTRAINT ux_scenarios_code UNIQUE (code);


--
-- Name: topics ux_topics_code; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.topics
    ADD CONSTRAINT ux_topics_code UNIQUE (code);


--
-- Name: idx_admin_audit_admin; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_admin_audit_admin ON public.admin_audit_logs USING btree (admin_user_id, id DESC);


--
-- Name: idx_admin_audit_created; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_admin_audit_created ON public.admin_audit_logs USING btree (id DESC);


--
-- Name: idx_admin_audit_target_user; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_admin_audit_target_user ON public.admin_audit_logs USING btree (target_user_id, id DESC) WHERE (target_user_id IS NOT NULL);


--
-- Name: idx_balance_ledger_reservation; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_balance_ledger_reservation ON public.balance_ledger USING btree (reservation_id) WHERE (reservation_id IS NOT NULL);


--
-- Name: idx_balance_ledger_session; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_balance_ledger_session ON public.balance_ledger USING btree (session_id) WHERE (session_id IS NOT NULL);


--
-- Name: idx_balance_ledger_user; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_balance_ledger_user ON public.balance_ledger USING btree (user_id, id DESC);


--
-- Name: idx_balance_reservations_held_deadline; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_balance_reservations_held_deadline ON public.balance_reservations USING btree (start_deadline_at) WHERE (status = 'HELD'::text);


--
-- Name: idx_balance_reservations_user; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_balance_reservations_user ON public.balance_reservations USING btree (user_id, created_at DESC);


--
-- Name: idx_characters_status_sort; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_characters_status_sort ON public.characters USING btree (status, sort_order);


--
-- Name: idx_evaluation_findings_evaluation; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_evaluation_findings_evaluation ON public.evaluation_findings USING btree (evaluation_id, ordinal);


--
-- Name: idx_evaluation_findings_user_pattern; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_evaluation_findings_user_pattern ON public.evaluation_findings USING btree (user_id, pattern_key, created_at DESC) WHERE (pattern_key IS NOT NULL);


--
-- Name: idx_evaluations_retention; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_evaluations_retention ON public.evaluations USING btree (completed_at) WHERE ((status = 'COMPLETED'::text) AND (details_purged_at IS NULL));


--
-- Name: idx_evaluations_status_updated; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_evaluations_status_updated ON public.evaluations USING btree (status, updated_at) WHERE (status = ANY (ARRAY['PENDING'::text, 'RUNNING'::text, 'FAILED'::text]));


--
-- Name: idx_evaluations_user_completed; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_evaluations_user_completed ON public.evaluations USING btree (user_id, completed_at DESC) WHERE (status = 'COMPLETED'::text);


--
-- Name: idx_event_outbox_aggregate; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_event_outbox_aggregate ON public.event_outbox USING btree (aggregate_type, aggregate_id);


--
-- Name: idx_event_outbox_failed; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_event_outbox_failed ON public.event_outbox USING btree (created_at) WHERE (status = 'FAILED'::text);


--
-- Name: idx_event_outbox_published_at; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_event_outbox_published_at ON public.event_outbox USING btree (published_at) WHERE (status = 'PUBLISHED'::text);


--
-- Name: idx_event_outbox_ready; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_event_outbox_ready ON public.event_outbox USING btree (available_at, event_id) WHERE (status = 'PENDING'::text);


--
-- Name: idx_lss_last_evaluation; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_lss_last_evaluation ON public.learner_skill_states USING btree (last_evaluation_id) WHERE (last_evaluation_id IS NOT NULL);


--
-- Name: idx_memories_expires_at; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_memories_expires_at ON public.memories USING btree (expires_at);


--
-- Name: idx_memories_source_session; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_memories_source_session ON public.memories USING btree (source_session_id) WHERE (source_session_id IS NOT NULL);


--
-- Name: idx_memories_superseded_by; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_memories_superseded_by ON public.memories USING btree (superseded_by) WHERE (superseded_by IS NOT NULL);


--
-- Name: idx_memories_user_status_cat; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_memories_user_status_cat ON public.memories USING btree (user_id, status, category, last_observed_at DESC);


--
-- Name: idx_password_reset_tokens_user_open; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_password_reset_tokens_user_open ON public.password_reset_tokens USING btree (user_id) WHERE ((used_at IS NULL) AND (invalidated_at IS NULL));


--
-- Name: idx_processed_events_processed_at; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_processed_events_processed_at ON public.processed_events USING btree (processed_at);


--
-- Name: idx_proficiency_estimates_session; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_proficiency_estimates_session ON public.proficiency_estimates USING btree (session_id);


--
-- Name: idx_proficiency_estimates_user_skill_time; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_proficiency_estimates_user_skill_time ON public.proficiency_estimates USING btree (user_id, skill, assessed_at DESC);


--
-- Name: idx_refresh_tokens_expires; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_refresh_tokens_expires ON public.refresh_tokens USING btree (expires_at);


--
-- Name: idx_refresh_tokens_family; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_refresh_tokens_family ON public.refresh_tokens USING btree (family_id);


--
-- Name: idx_refresh_tokens_user_active; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_refresh_tokens_user_active ON public.refresh_tokens USING btree (user_id) WHERE (revoked_at IS NULL);


--
-- Name: idx_scenarios_topic_status_sort; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_scenarios_topic_status_sort ON public.scenarios USING btree (topic_id, status, sort_order);


--
-- Name: idx_session_events_created; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_session_events_created ON ONLY public.session_events USING btree (created_at);


--
-- Name: idx_session_events_session; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_session_events_session ON ONLY public.session_events USING btree (session_id, created_at, id);


--
-- Name: idx_session_turns_created; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_session_turns_created ON ONLY public.session_turns USING btree (created_at);


--
-- Name: idx_session_turns_open; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_session_turns_open ON ONLY public.session_turns USING btree (session_id) WHERE (status IS NULL);


--
-- Name: idx_sessions_active_expires; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_sessions_active_expires ON public.sessions USING btree (expires_at) WHERE (status = 'ACTIVE'::text);


--
-- Name: idx_sessions_ending; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_sessions_ending ON public.sessions USING btree (ending_at) WHERE (status = 'ENDING'::text);


--
-- Name: idx_sessions_handoff; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_sessions_handoff ON public.sessions USING btree (handoff_at) WHERE ((status = 'ACTIVE'::text) AND (handoff_at IS NOT NULL));


--
-- Name: idx_sessions_retention; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_sessions_retention ON public.sessions USING btree (ended_at) WHERE ((ended_at IS NOT NULL) AND (transcript_purged_at IS NULL));


--
-- Name: idx_sessions_starting; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_sessions_starting ON public.sessions USING btree (starting_at) WHERE (status = 'STARTING'::text);


--
-- Name: idx_sessions_summary_retention; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_sessions_summary_retention ON public.sessions USING btree (ended_at) WHERE (summary_text IS NOT NULL);


--
-- Name: idx_sessions_user_id_desc; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_sessions_user_id_desc ON public.sessions USING btree (user_id, id DESC);


--
-- Name: idx_topics_status_sort; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_topics_status_sort ON public.topics USING btree (status, sort_order);


--
-- Name: idx_users_purge_pending; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_users_purge_pending ON public.users USING btree (deleted_at) WHERE ((status = 'DELETED'::text) AND (purged_at IS NULL));


--
-- Name: idx_users_status_created; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_users_status_created ON public.users USING btree (status, id DESC);


--
-- Name: ux_memories_active_slot; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX ux_memories_active_slot ON public.memories USING btree (user_id, category, memory_key) WHERE (status = 'ACTIVE'::text);


--
-- Name: ux_memories_tombstone_slot; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX ux_memories_tombstone_slot ON public.memories USING btree (user_id, category, memory_key) WHERE (status = 'DELETED'::text);


--
-- Name: ux_password_reset_tokens_hash; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX ux_password_reset_tokens_hash ON public.password_reset_tokens USING btree (token_hash);


--
-- Name: ux_prompt_versions_published; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX ux_prompt_versions_published ON public.prompt_versions USING btree (prompt_config_id) WHERE (status = 'PUBLISHED'::text);


--
-- Name: ux_refresh_tokens_hash; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX ux_refresh_tokens_hash ON public.refresh_tokens USING btree (token_hash);


--
-- Name: ux_scenario_versions_published; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX ux_scenario_versions_published ON public.scenario_versions USING btree (scenario_id) WHERE (status = 'PUBLISHED'::text);


--
-- Name: ux_sessions_one_open_per_user; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX ux_sessions_one_open_per_user ON public.sessions USING btree (user_id) WHERE (status = ANY (ARRAY['RESERVED'::text, 'STARTING'::text, 'ACTIVE'::text, 'ENDING'::text]));


--
-- Name: ux_sessions_user_idempotency; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX ux_sessions_user_idempotency ON public.sessions USING btree (user_id, create_idempotency_key);


--
-- Name: ux_users_email; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX ux_users_email ON public.users USING btree (email);


--
-- Name: ux_users_phone; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX ux_users_phone ON public.users USING btree (phone_number) WHERE (phone_number IS NOT NULL);


--
-- Name: admin_audit_logs trg_admin_audit_logs_append_only; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER trg_admin_audit_logs_append_only BEFORE DELETE OR UPDATE ON public.admin_audit_logs FOR EACH ROW EXECUTE FUNCTION public.reject_mutation();


--
-- Name: balance_ledger trg_balance_ledger_append_only; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER trg_balance_ledger_append_only BEFORE DELETE OR UPDATE ON public.balance_ledger FOR EACH ROW EXECUTE FUNCTION public.reject_mutation();


--
-- Name: prompt_versions trg_prompt_versions_guard; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER trg_prompt_versions_guard BEFORE DELETE OR UPDATE ON public.prompt_versions FOR EACH ROW EXECUTE FUNCTION public.content_version_guard();


--
-- Name: scenario_versions trg_scenario_versions_guard; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER trg_scenario_versions_guard BEFORE DELETE OR UPDATE ON public.scenario_versions FOR EACH ROW EXECUTE FUNCTION public.content_version_guard();


--
-- Name: admin_audit_logs admin_audit_logs_admin_user_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.admin_audit_logs
    ADD CONSTRAINT admin_audit_logs_admin_user_id_fkey FOREIGN KEY (admin_user_id) REFERENCES public.users(id) ON DELETE RESTRICT;


--
-- Name: admin_audit_logs admin_audit_logs_target_user_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.admin_audit_logs
    ADD CONSTRAINT admin_audit_logs_target_user_id_fkey FOREIGN KEY (target_user_id) REFERENCES public.users(id) ON DELETE RESTRICT;


--
-- Name: balance_accounts balance_accounts_user_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.balance_accounts
    ADD CONSTRAINT balance_accounts_user_id_fkey FOREIGN KEY (user_id) REFERENCES public.users(id) ON DELETE RESTRICT;


--
-- Name: balance_ledger balance_ledger_account_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.balance_ledger
    ADD CONSTRAINT balance_ledger_account_id_fkey FOREIGN KEY (account_id) REFERENCES public.balance_accounts(id) ON DELETE RESTRICT;


--
-- Name: balance_ledger balance_ledger_admin_user_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.balance_ledger
    ADD CONSTRAINT balance_ledger_admin_user_id_fkey FOREIGN KEY (admin_user_id) REFERENCES public.users(id) ON DELETE RESTRICT;


--
-- Name: balance_ledger balance_ledger_reservation_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.balance_ledger
    ADD CONSTRAINT balance_ledger_reservation_id_fkey FOREIGN KEY (reservation_id) REFERENCES public.balance_reservations(id) ON DELETE RESTRICT;


--
-- Name: balance_ledger balance_ledger_session_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.balance_ledger
    ADD CONSTRAINT balance_ledger_session_id_fkey FOREIGN KEY (session_id) REFERENCES public.sessions(id) ON DELETE RESTRICT;


--
-- Name: balance_ledger balance_ledger_user_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.balance_ledger
    ADD CONSTRAINT balance_ledger_user_id_fkey FOREIGN KEY (user_id) REFERENCES public.users(id) ON DELETE RESTRICT;


--
-- Name: balance_reservations balance_reservations_account_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.balance_reservations
    ADD CONSTRAINT balance_reservations_account_id_fkey FOREIGN KEY (account_id) REFERENCES public.balance_accounts(id) ON DELETE RESTRICT;


--
-- Name: balance_reservations balance_reservations_session_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.balance_reservations
    ADD CONSTRAINT balance_reservations_session_id_fkey FOREIGN KEY (session_id) REFERENCES public.sessions(id) ON DELETE RESTRICT;


--
-- Name: balance_reservations balance_reservations_user_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.balance_reservations
    ADD CONSTRAINT balance_reservations_user_id_fkey FOREIGN KEY (user_id) REFERENCES public.users(id) ON DELETE RESTRICT;


--
-- Name: evaluation_findings evaluation_findings_evaluation_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.evaluation_findings
    ADD CONSTRAINT evaluation_findings_evaluation_id_fkey FOREIGN KEY (evaluation_id) REFERENCES public.evaluations(id) ON DELETE CASCADE;


--
-- Name: evaluation_findings evaluation_findings_session_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.evaluation_findings
    ADD CONSTRAINT evaluation_findings_session_id_fkey FOREIGN KEY (session_id) REFERENCES public.sessions(id) ON DELETE CASCADE;


--
-- Name: evaluation_findings evaluation_findings_user_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.evaluation_findings
    ADD CONSTRAINT evaluation_findings_user_id_fkey FOREIGN KEY (user_id) REFERENCES public.users(id) ON DELETE CASCADE;


--
-- Name: evaluations evaluations_prompt_version_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.evaluations
    ADD CONSTRAINT evaluations_prompt_version_id_fkey FOREIGN KEY (prompt_version_id) REFERENCES public.prompt_versions(id);


--
-- Name: evaluations evaluations_session_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.evaluations
    ADD CONSTRAINT evaluations_session_id_fkey FOREIGN KEY (session_id) REFERENCES public.sessions(id) ON DELETE CASCADE;


--
-- Name: evaluations evaluations_user_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.evaluations
    ADD CONSTRAINT evaluations_user_id_fkey FOREIGN KEY (user_id) REFERENCES public.users(id) ON DELETE CASCADE;


--
-- Name: learner_skill_states fk_lss_last_evaluation; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.learner_skill_states
    ADD CONSTRAINT fk_lss_last_evaluation FOREIGN KEY (last_evaluation_id) REFERENCES public.evaluations(id) ON DELETE SET NULL;


--
-- Name: proficiency_estimates fk_pe_source_evaluation; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.proficiency_estimates
    ADD CONSTRAINT fk_pe_source_evaluation FOREIGN KEY (source_evaluation_id) REFERENCES public.evaluations(id) ON DELETE CASCADE;


--
-- Name: prompt_versions fk_prompt_versions_config; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.prompt_versions
    ADD CONSTRAINT fk_prompt_versions_config FOREIGN KEY (prompt_config_id) REFERENCES public.prompt_configs(id) ON DELETE RESTRICT;


--
-- Name: scenario_versions fk_scenario_versions_scenario; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.scenario_versions
    ADD CONSTRAINT fk_scenario_versions_scenario FOREIGN KEY (scenario_id) REFERENCES public.scenarios(id) ON DELETE RESTRICT;


--
-- Name: scenarios fk_scenarios_topic; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.scenarios
    ADD CONSTRAINT fk_scenarios_topic FOREIGN KEY (topic_id) REFERENCES public.topics(id) ON DELETE RESTRICT;


--
-- Name: learner_profiles learner_profiles_user_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.learner_profiles
    ADD CONSTRAINT learner_profiles_user_id_fkey FOREIGN KEY (user_id) REFERENCES public.users(id) ON DELETE CASCADE;


--
-- Name: learner_skill_states learner_skill_states_user_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.learner_skill_states
    ADD CONSTRAINT learner_skill_states_user_id_fkey FOREIGN KEY (user_id) REFERENCES public.learner_profiles(user_id) ON DELETE CASCADE;


--
-- Name: memories memories_extraction_prompt_version_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.memories
    ADD CONSTRAINT memories_extraction_prompt_version_id_fkey FOREIGN KEY (extraction_prompt_version_id) REFERENCES public.prompt_versions(id);


--
-- Name: memories memories_source_session_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.memories
    ADD CONSTRAINT memories_source_session_id_fkey FOREIGN KEY (source_session_id) REFERENCES public.sessions(id) ON DELETE SET NULL;


--
-- Name: memories memories_superseded_by_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.memories
    ADD CONSTRAINT memories_superseded_by_fkey FOREIGN KEY (superseded_by) REFERENCES public.memories(id) ON DELETE SET NULL DEFERRABLE INITIALLY DEFERRED;


--
-- Name: memories memories_user_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.memories
    ADD CONSTRAINT memories_user_id_fkey FOREIGN KEY (user_id) REFERENCES public.users(id) ON DELETE CASCADE;


--
-- Name: password_reset_tokens password_reset_tokens_user_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.password_reset_tokens
    ADD CONSTRAINT password_reset_tokens_user_id_fkey FOREIGN KEY (user_id) REFERENCES public.users(id) ON DELETE RESTRICT;


--
-- Name: proficiency_estimates proficiency_estimates_session_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.proficiency_estimates
    ADD CONSTRAINT proficiency_estimates_session_id_fkey FOREIGN KEY (session_id) REFERENCES public.sessions(id) ON DELETE CASCADE;


--
-- Name: proficiency_estimates proficiency_estimates_user_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.proficiency_estimates
    ADD CONSTRAINT proficiency_estimates_user_id_fkey FOREIGN KEY (user_id) REFERENCES public.learner_profiles(user_id) ON DELETE CASCADE;


--
-- Name: refresh_tokens refresh_tokens_parent_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.refresh_tokens
    ADD CONSTRAINT refresh_tokens_parent_id_fkey FOREIGN KEY (parent_id) REFERENCES public.refresh_tokens(id);


--
-- Name: refresh_tokens refresh_tokens_replaced_by_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.refresh_tokens
    ADD CONSTRAINT refresh_tokens_replaced_by_id_fkey FOREIGN KEY (replaced_by_id) REFERENCES public.refresh_tokens(id);


--
-- Name: refresh_tokens refresh_tokens_user_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.refresh_tokens
    ADD CONSTRAINT refresh_tokens_user_id_fkey FOREIGN KEY (user_id) REFERENCES public.users(id) ON DELETE RESTRICT;


--
-- Name: session_events session_events_session_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE public.session_events
    ADD CONSTRAINT session_events_session_id_fkey FOREIGN KEY (session_id) REFERENCES public.sessions(id) ON DELETE RESTRICT;


--
-- Name: session_turns session_turns_session_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE public.session_turns
    ADD CONSTRAINT session_turns_session_id_fkey FOREIGN KEY (session_id) REFERENCES public.sessions(id) ON DELETE RESTRICT;


--
-- Name: sessions sessions_character_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.sessions
    ADD CONSTRAINT sessions_character_id_fkey FOREIGN KEY (character_id) REFERENCES public.characters(id) ON DELETE RESTRICT;


--
-- Name: sessions sessions_opening_prompt_version_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.sessions
    ADD CONSTRAINT sessions_opening_prompt_version_id_fkey FOREIGN KEY (opening_prompt_version_id) REFERENCES public.prompt_versions(id) ON DELETE RESTRICT;


--
-- Name: sessions sessions_prompt_version_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.sessions
    ADD CONSTRAINT sessions_prompt_version_id_fkey FOREIGN KEY (prompt_version_id) REFERENCES public.prompt_versions(id) ON DELETE RESTRICT;


--
-- Name: sessions sessions_scenario_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.sessions
    ADD CONSTRAINT sessions_scenario_id_fkey FOREIGN KEY (scenario_id) REFERENCES public.scenarios(id) ON DELETE RESTRICT;


--
-- Name: sessions sessions_scenario_version_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.sessions
    ADD CONSTRAINT sessions_scenario_version_id_fkey FOREIGN KEY (scenario_version_id) REFERENCES public.scenario_versions(id) ON DELETE RESTRICT;


--
-- Name: sessions sessions_topic_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.sessions
    ADD CONSTRAINT sessions_topic_id_fkey FOREIGN KEY (topic_id) REFERENCES public.topics(id) ON DELETE RESTRICT;


--
-- Name: sessions sessions_user_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.sessions
    ADD CONSTRAINT sessions_user_id_fkey FOREIGN KEY (user_id) REFERENCES public.users(id) ON DELETE RESTRICT;


--
-- PostgreSQL database dump complete
--
