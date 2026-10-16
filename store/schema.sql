-- store/schema.sql is the collapsed snapshot of the production
-- schema. Regenerate with `make collapse` after a successful prod
-- deploy. Applied automatically to a virgin DB before goose runs the
-- migrations/ directory; goose then sees the collapsed migrations
-- already recorded in goose_db_version and only applies post-
-- collapse deltas.
--
-- See docs/plans/migration-collapse.md for the design.

--
-- PostgreSQL database dump
--

\restrict xcqB3V4xpJgLH7UDHdnpVyG3qp0zTmBo6LFUTmNd2DfWLF7JUBoReX7jBx7qwSa

-- Dumped from database version 16.15 (Debian 16.15-1.pgdg13+2)
-- Dumped by pg_dump version 16.15 (Debian 16.15-1.pgdg13+2)

SET statement_timeout = 0;
SET lock_timeout = 0;
SET idle_in_transaction_session_timeout = 0;
SET client_encoding = 'UTF8';
SET standard_conforming_strings = on;
SELECT pg_catalog.set_config('search_path', '', false);
SET check_function_bodies = false;
SET xmloption = content;
SET client_min_messages = warning;
SET row_security = off;

--
-- Name: pg_cron; Type: EXTENSION; Schema: -; Owner: -
--

CREATE EXTENSION IF NOT EXISTS pg_cron WITH SCHEMA pg_catalog;


--
-- Name: partman; Type: SCHEMA; Schema: -; Owner: -
--

CREATE SCHEMA partman;


--
-- Name: pg_partman; Type: EXTENSION; Schema: -; Owner: -
--

CREATE EXTENSION IF NOT EXISTS pg_partman WITH SCHEMA partman;


--
-- Name: pgcrypto; Type: EXTENSION; Schema: -; Owner: -
--

CREATE EXTENSION IF NOT EXISTS pgcrypto WITH SCHEMA public;


--
-- Name: log_group_source_type; Type: TYPE; Schema: public; Owner: -
--

CREATE TYPE public.log_group_source_type AS ENUM (
    'cloudwatch',
    'weftd',
    'direct',
    'kinesis'
);


--
-- Name: notice_kind; Type: TYPE; Schema: public; Owner: -
--

CREATE TYPE public.notice_kind AS ENUM (
    'success',
    'warning',
    'error'
);


--
-- Name: capture_cron_watch_history(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.capture_cron_watch_history() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    last_history_id BIGINT;
    captured_by     TEXT;
    history_count   INT;
    revived         BOOLEAN;
BEGIN
    -- No-op save (open/close in :w without editing): the row content
    -- is unchanged, so don't pollute history with a duplicate hash. A revive
    -- (deleted_at cleared) is never a no-op: the deletion has to be kept.
    revived := OLD.deleted_at IS NOT NULL AND NEW.deleted_at IS NULL;
    IF OLD.content_hash = NEW.content_hash AND NOT revived THEN
        RETURN NEW;
    END IF;

    captured_by := current_setting('weft.acting_user_id', true);
    IF captured_by = '' THEN
        captured_by := NULL;
    END IF;

    SELECT id INTO last_history_id
    FROM cron_watch_history
    WHERE watch_id = OLD.id
    ORDER BY captured_at DESC, id DESC
    LIMIT 1;

    INSERT INTO cron_watch_history (
        watch_id, org_id, name, schedule, pipeline, content_hash,
        message, captured_by_user_id, authored_by_user_id, predecessor_id,
        version_id, authored_at, deleted_at
    ) VALUES (
        OLD.id, OLD.org_id, OLD.name, OLD.schedule, OLD.pipeline,
        OLD.content_hash, OLD.message, captured_by,
        OLD.authored_by_user_id, last_history_id,
        OLD.version_id, OLD.authored_at, OLD.deleted_at
    );

    NEW.version_id := nextval('public.object_version_seq');
    NEW.authored_at := clock_timestamp();
    NEW.authored_by_user_id := captured_by;
    NEW.message := '';

    SELECT count(*) INTO history_count
    FROM cron_watch_history
    WHERE watch_id = OLD.id;

    IF history_count > 5 THEN
        DELETE FROM cron_watch_history
        WHERE id = (
            SELECT id FROM cron_watch_history
            WHERE watch_id = OLD.id
            ORDER BY captured_at ASC, id ASC
            LIMIT 1
        );
    END IF;

    RETURN NEW;
END;
$$;


--
-- Name: capture_dashboard_history(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.capture_dashboard_history() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    last_history_id BIGINT;
    captured_by     TEXT;
BEGIN
    IF OLD.yaml_body = NEW.yaml_body THEN
        RETURN NEW;
    END IF;

    captured_by := NULLIF(current_setting('weft.acting_user_id', true), '');

    SELECT id INTO last_history_id FROM dashboard_history
     WHERE dashboard_id = OLD.id ORDER BY captured_at DESC, id DESC LIMIT 1;

    INSERT INTO dashboard_history (
        dashboard_id, org_id, name, yaml_body, message, captured_by_user_id,
        authored_by_user_id, predecessor_id, version_id, authored_at
    ) VALUES (
        OLD.id, OLD.org_id, OLD.name, OLD.yaml_body, OLD.message, captured_by,
        OLD.authored_by_user_id, last_history_id, OLD.version_id, OLD.authored_at
    );

    NEW.version_id := nextval('public.object_version_seq');
    NEW.authored_at := clock_timestamp();
    NEW.authored_by_user_id := captured_by;
    NEW.message := '';

    IF (SELECT count(*) FROM dashboard_history WHERE dashboard_id = OLD.id) > 5 THEN
        DELETE FROM dashboard_history WHERE id = (
            SELECT id FROM dashboard_history WHERE dashboard_id = OLD.id
             ORDER BY captured_at ASC, id ASC LIMIT 1);
    END IF;
    RETURN NEW;
END;
$$;


--
-- Name: capture_notify_config_history(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.capture_notify_config_history() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    last_history_id BIGINT;
    captured_by     TEXT;
BEGIN
    IF OLD.aliases_yaml = NEW.aliases_yaml THEN
        RETURN NEW;
    END IF;

    captured_by := NULLIF(current_setting('weft.acting_user_id', true), '');

    SELECT id INTO last_history_id FROM notify_config_history
     WHERE notify_config_id = OLD.id ORDER BY captured_at DESC, id DESC LIMIT 1;

    INSERT INTO notify_config_history (
        notify_config_id, org_id, aliases_yaml, message, captured_by_user_id,
        authored_by_user_id, predecessor_id, version_id, authored_at
    ) VALUES (
        OLD.id, OLD.org_id, OLD.aliases_yaml, OLD.message, captured_by,
        OLD.authored_by_user_id, last_history_id, OLD.version_id, OLD.authored_at
    );

    NEW.version_id := nextval('public.object_version_seq');
    NEW.authored_at := clock_timestamp();
    NEW.authored_by_user_id := captured_by;
    NEW.message := '';

    IF (SELECT count(*) FROM notify_config_history WHERE notify_config_id = OLD.id) > 5 THEN
        DELETE FROM notify_config_history WHERE id = (
            SELECT id FROM notify_config_history WHERE notify_config_id = OLD.id
             ORDER BY captured_at ASC, id ASC LIMIT 1);
    END IF;
    RETURN NEW;
END;
$$;


--
-- Name: capture_shell_function_history(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.capture_shell_function_history() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    last_history_id BIGINT;
    captured_by     TEXT;
    history_count   INT;
    revived         BOOLEAN;
BEGIN
    -- No-op save (open/close in :w without editing): the row content
    -- is unchanged, so don't pollute history with a duplicate hash. A revive
    -- (deleted_at cleared) is never a no-op: the deletion has to be kept.
    revived := OLD.deleted_at IS NOT NULL AND NEW.deleted_at IS NULL;
    IF OLD.content_hash = NEW.content_hash AND NOT revived THEN
        RETURN NEW;
    END IF;

    captured_by := current_setting('weft.acting_user_id', true);
    IF captured_by = '' THEN
        captured_by := NULL;
    END IF;

    SELECT id INTO last_history_id
    FROM shell_function_history
    WHERE function_id = OLD.id
    ORDER BY captured_at DESC, id DESC
    LIMIT 1;

    INSERT INTO shell_function_history (
        function_id, org_id, name, source, language, content_hash,
        message, captured_by_user_id, authored_by_user_id, predecessor_id,
        version_id, authored_at, deleted_at
    ) VALUES (
        OLD.id, OLD.org_id, OLD.name, OLD.source, OLD.language,
        OLD.content_hash, OLD.message, captured_by,
        OLD.authored_by_user_id, last_history_id,
        OLD.version_id, OLD.authored_at, OLD.deleted_at
    );

    NEW.version_id := nextval('public.object_version_seq');
    NEW.authored_at := clock_timestamp();
    NEW.authored_by_user_id := captured_by;
    NEW.message := '';

    SELECT count(*) INTO history_count
    FROM shell_function_history
    WHERE function_id = OLD.id;

    IF history_count > 5 THEN
        DELETE FROM shell_function_history
        WHERE id = (
            SELECT id FROM shell_function_history
            WHERE function_id = OLD.id
            ORDER BY captured_at ASC, id ASC
            LIMIT 1
        );
    END IF;

    RETURN NEW;
END;
$$;


--
-- Name: etc_set_origin_on_insert(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.etc_set_origin_on_insert() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    NEW.origin_version_id := NEW.version_id;
    NEW.authored_by_user_id := NULLIF(current_setting('weft.acting_user_id', true), '');
    RETURN NEW;
END;
$$;


--
-- Name: log_groups_fill_org_id(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.log_groups_fill_org_id() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
  IF NEW.org_id IS NULL THEN
    SELECT org_id INTO NEW.org_id FROM namespaces WHERE id = NEW.namespace_id;
  END IF;
  RETURN NEW;
END;
$$;


--
-- Name: log_shard_operation(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.log_shard_operation() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        INSERT INTO shard_operations (shard_id, from_state, to_state)
        VALUES (NEW.id, NULL, NEW.state);
        RETURN NEW;
    ELSIF TG_OP = 'UPDATE' THEN
        IF NEW.state IS DISTINCT FROM OLD.state THEN
            INSERT INTO shard_operations (shard_id, from_state, to_state)
            VALUES (NEW.id, OLD.state, NEW.state);
        END IF;
        RETURN NEW;
    ELSIF TG_OP = 'DELETE' THEN
        INSERT INTO shard_operations (shard_id, from_state, to_state)
        VALUES (OLD.id, OLD.state, NULL);
        RETURN OLD;
    END IF;
    RETURN NULL;
END;
$$;


--
-- Name: rollup_cron_4h(); Type: PROCEDURE; Schema: public; Owner: -
--

CREATE PROCEDURE public.rollup_cron_4h()
    LANGUAGE sql
    AS $$
    INSERT INTO public.cron_results_4h
        (cron_watch_id, tag, bucket_start, count, sum, sum_sq, min, max)
    SELECT
        cron_watch_id,
        tag,
        date_trunc('day', bucket_start)
          + INTERVAL '4 hours' * FLOOR(EXTRACT(hour FROM bucket_start) / 4)
                                              AS bucket_4h,
        SUM(count)                            AS count,
        SUM(sum)                              AS sum,
        SUM(sum_sq)                           AS sum_sq,
        MIN(min)                              AS min,
        MAX(max)                              AS max
    FROM public.cron_results_hourly
    WHERE bucket_start >= date_trunc('hour', now()) - INTERVAL '12 hours'
      AND bucket_start <  date_trunc('hour', now()) - INTERVAL '1 hour'
    GROUP BY cron_watch_id, tag,
             date_trunc('day', bucket_start)
               + INTERVAL '4 hours' * FLOOR(EXTRACT(hour FROM bucket_start) / 4)
    ON CONFLICT (cron_watch_id, tag, bucket_start) DO UPDATE SET
        count  = EXCLUDED.count,
        sum    = EXCLUDED.sum,
        sum_sq = EXCLUDED.sum_sq,
        min    = EXCLUDED.min,
        max    = EXCLUDED.max;
$$;


--
-- Name: rollup_cron_daily(); Type: PROCEDURE; Schema: public; Owner: -
--

CREATE PROCEDURE public.rollup_cron_daily()
    LANGUAGE sql
    AS $$
    INSERT INTO public.cron_results_daily
        (cron_watch_id, tag, bucket_start, count, sum, sum_sq, min, max)
    SELECT
        cron_watch_id,
        tag,
        date_trunc('day', bucket_start)       AS bucket_day,
        SUM(count)                            AS count,
        SUM(sum)                              AS sum,
        SUM(sum_sq)                           AS sum_sq,
        MIN(min)                              AS min,
        MAX(max)                              AS max
    FROM public.cron_results_4h
    WHERE bucket_start >= date_trunc('day', now()) - INTERVAL '3 days'
      AND bucket_start <  date_trunc('day', now())
    GROUP BY cron_watch_id, tag, date_trunc('day', bucket_start)
    ON CONFLICT (cron_watch_id, tag, bucket_start) DO UPDATE SET
        count  = EXCLUDED.count,
        sum    = EXCLUDED.sum,
        sum_sq = EXCLUDED.sum_sq,
        min    = EXCLUDED.min,
        max    = EXCLUDED.max;
$$;


--
-- Name: rollup_cron_hourly(); Type: PROCEDURE; Schema: public; Owner: -
--

CREATE PROCEDURE public.rollup_cron_hourly()
    LANGUAGE sql
    AS $$
    INSERT INTO public.cron_results_hourly
        (cron_watch_id, tag, bucket_start, count, sum, sum_sq, min, max)
    SELECT
        cron_watch_id,
        tag,
        date_trunc('hour', ran_at)          AS bucket_start,
        COUNT(*)                            AS count,
        SUM(value)                          AS sum,
        SUM(value * value)                  AS sum_sq,
        MIN(value)                          AS min,
        MAX(value)                          AS max
    FROM public.cron_results
    WHERE ran_at >= date_trunc('hour', now()) - INTERVAL '3 hours'
      AND ran_at <  date_trunc('hour', now())
      AND value IS NOT NULL
    GROUP BY cron_watch_id, tag, date_trunc('hour', ran_at)
    ON CONFLICT (cron_watch_id, tag, bucket_start) DO UPDATE SET
        count  = EXCLUDED.count,
        sum    = EXCLUDED.sum,
        sum_sq = EXCLUDED.sum_sq,
        min    = EXCLUDED.min,
        max    = EXCLUDED.max;
$$;


--
-- Name: rollup_usage_daily(); Type: PROCEDURE; Schema: public; Owner: -
--

CREATE PROCEDURE public.rollup_usage_daily()
    LANGUAGE sql
    AS $$
    INSERT INTO public.usage_daily (org_id, day, bytes_ingested)
    SELECT org_id,
           date_trunc('day', hour)::date AS day,
           SUM(bytes_ingested)           AS bytes_ingested
    FROM public.usage_hourly
    WHERE hour >= date_trunc('day', now()) - INTERVAL '3 days'
    GROUP BY org_id, date_trunc('day', hour)::date
    ON CONFLICT (org_id, day) DO UPDATE SET
        bytes_ingested = EXCLUDED.bytes_ingested;

    DELETE FROM public.usage_hourly
    WHERE hour < date_trunc('day', now()) - INTERVAL '14 days';
$$;


--
-- Name: set_authored_by_on_insert(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.set_authored_by_on_insert() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    NEW.authored_by_user_id := NEW.created_by_user_id;
    NEW.origin_version_id := NEW.version_id;
    RETURN NEW;
END;
$$;


--
-- Name: sync_membership_org_permissions(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.sync_membership_org_permissions() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    perm     public.org_permissions%ROWTYPE;
    retained text[];
    actor    text;
    subject  text;
    org_name text;
BEGIN
    -- A pending invite has no user to grant anything to.
    IF NEW.user_id IS NULL THEN
        RETURN NEW;
    END IF;

    -- Removal tombstones the row. Re-adding the member restores the
    -- membership but not the grants; they are re-granted explicitly.
    IF NEW.status = 'removed' THEN
        UPDATE public.org_permissions
           SET deleted_at = now(),
               updated_at = now()
         WHERE org_id = NEW.org_id
           AND user_id = NEW.user_id
           AND deleted_at IS NULL;
        RETURN NEW;
    END IF;

    -- Promotion, or an admin becoming active: all four bits, explicitly.
    -- Idempotent, so it is safe on any UPDATE that merely touches role or
    -- status without changing them.
    IF NEW.role = 'admin' AND NEW.status = 'active' THEN
        INSERT INTO public.org_permissions
                (org_id, user_id, can_manage_functions, can_manage_watches,
                 can_manage_etc, can_create_namespaces, created_at, updated_at)
        VALUES (NEW.org_id, NEW.user_id, true, true, true, true, now(), now())
        ON CONFLICT (org_id, user_id) DO UPDATE
            SET can_manage_functions  = true,
                can_manage_watches    = true,
                can_manage_etc        = true,
                can_create_namespaces = true,
                deleted_at            = NULL,
                updated_at            = now();
        RETURN NEW;
    END IF;

    -- Demotion. The bits stay; the operator is told what stayed.
    IF TG_OP <> 'UPDATE' OR OLD.role <> 'admin' OR NEW.role = 'admin' THEN
        RETURN NEW;
    END IF;

    SELECT * INTO perm
      FROM public.org_permissions
     WHERE org_id = NEW.org_id
       AND user_id = NEW.user_id
       AND deleted_at IS NULL;
    IF NOT FOUND THEN
        RETURN NEW;
    END IF;

    retained := ARRAY(
        SELECT g FROM (VALUES
            ('binmgr',    perm.can_manage_functions),
            ('cronmgr',   perm.can_manage_watches),
            ('configmgr', perm.can_manage_etc),
            ('nsmgr',     perm.can_create_namespaces)
        ) AS t(g, held) WHERE held
    );
    IF cardinality(retained) = 0 THEN
        RETURN NEW;
    END IF;

    -- Who to tell. set_config(..., true) is transaction-local, so this is
    -- only non-empty when the caller set it on this very connection. An
    -- out-of-band change (psql, a script) has no operator to inform, and
    -- silence is the correct outcome there — not an error.
    actor := coalesce(current_setting('weft.acting_user_id', true), '');
    IF actor = '' THEN
        RETURN NEW;
    END IF;
    -- user_notices.user_id is FK'd to users. A stale or bogus GUC must not
    -- take down a membership update, so check before inserting.
    IF NOT EXISTS (SELECT 1 FROM public.users WHERE id = actor) THEN
        RETURN NEW;
    END IF;

    subject := coalesce(
        NEW.email,
        (SELECT email FROM public.users WHERE id = NEW.user_id),
        NEW.user_id);

    -- Name the org. Notices are addressed to a user, not to a (user, org)
    -- pair, and the operator can be reading with any of their memberships
    -- selected in the switcher — so an org-scoped fact that doesn't say
    -- which org is a riddle. The renderer leads with it.
    SELECT name INTO org_name FROM public.organizations WHERE id = NEW.org_id;

    INSERT INTO public.user_notices (user_id, notice_key, kind, params, created_at)
    VALUES (actor,
            'member_retained_grants',
            'warning'::public.notice_kind,
            jsonb_build_object(
                'org',    coalesce(org_name, NEW.org_id::text),
                'member', subject,
                'groups', to_jsonb(retained)),
            now());

    RETURN NEW;
END;
$$;


SET default_tablespace = '';

SET default_table_access_method = heap;

--
-- Name: template_public_cron_results; Type: TABLE; Schema: partman; Owner: -
--

CREATE TABLE partman.template_public_cron_results (
    id bigint NOT NULL,
    created_at timestamp with time zone,
    updated_at timestamp with time zone,
    deleted_at timestamp with time zone,
    cron_watch_id bigint NOT NULL,
    ran_at timestamp with time zone NOT NULL,
    stdout text,
    stderr text,
    exit_code bigint,
    duration_ms bigint,
    value double precision
);


--
-- Name: template_public_cron_results_4h; Type: TABLE; Schema: partman; Owner: -
--

CREATE TABLE partman.template_public_cron_results_4h (
    cron_watch_id bigint NOT NULL,
    tag text NOT NULL,
    bucket_start timestamp with time zone NOT NULL,
    count bigint NOT NULL,
    sum double precision NOT NULL,
    sum_sq double precision NOT NULL,
    min double precision NOT NULL,
    max double precision NOT NULL
);


--
-- Name: template_public_cron_results_daily; Type: TABLE; Schema: partman; Owner: -
--

CREATE TABLE partman.template_public_cron_results_daily (
    cron_watch_id bigint NOT NULL,
    tag text NOT NULL,
    bucket_start timestamp with time zone NOT NULL,
    count bigint NOT NULL,
    sum double precision NOT NULL,
    sum_sq double precision NOT NULL,
    min double precision NOT NULL,
    max double precision NOT NULL
);


--
-- Name: template_public_cron_results_hourly; Type: TABLE; Schema: partman; Owner: -
--

CREATE TABLE partman.template_public_cron_results_hourly (
    cron_watch_id bigint NOT NULL,
    tag text NOT NULL,
    bucket_start timestamp with time zone NOT NULL,
    count bigint NOT NULL,
    sum double precision NOT NULL,
    sum_sq double precision NOT NULL,
    min double precision NOT NULL,
    max double precision NOT NULL
);


--
-- Name: template_public_tmp_file_lines; Type: TABLE; Schema: partman; Owner: -
--

CREATE TABLE partman.template_public_tmp_file_lines (
    file_id bigint NOT NULL,
    line_num bigint NOT NULL,
    content text NOT NULL,
    updated_at timestamp with time zone NOT NULL
);


--
-- Name: template_public_tmp_files; Type: TABLE; Schema: partman; Owner: -
--

CREATE TABLE partman.template_public_tmp_files (
    id bigint NOT NULL,
    path text NOT NULL,
    is_dir boolean NOT NULL,
    byte_size bigint NOT NULL,
    line_count bigint NOT NULL,
    created_at timestamp with time zone NOT NULL,
    updated_at timestamp with time zone NOT NULL,
    org_id bigint
);


--
-- Name: agent_tokens; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.agent_tokens (
    id bigint NOT NULL,
    created_at timestamp with time zone,
    updated_at timestamp with time zone,
    deleted_at timestamp with time zone,
    namespace_id bigint NOT NULL,
    token_hash text NOT NULL,
    label text,
    revoked boolean DEFAULT false NOT NULL,
    metrics_enabled boolean DEFAULT false NOT NULL,
    metrics_interval text,
    metrics_collectors text,
    prefix text NOT NULL,
    env text DEFAULT ''::text NOT NULL,
    last_seen_version text DEFAULT ''::text NOT NULL,
    last_seen_at timestamp with time zone,
    org_id bigint NOT NULL,
    public_id text NOT NULL
);


--
-- Name: agent_tokens_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.agent_tokens_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: agent_tokens_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.agent_tokens_id_seq OWNED BY public.agent_tokens.id;


--
-- Name: alert_incidents; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.alert_incidents (
    id text NOT NULL,
    cron_watch_id bigint NOT NULL,
    condition text NOT NULL,
    opened_at timestamp with time zone NOT NULL,
    closed_at timestamp with time zone,
    last_seen_at timestamp with time zone NOT NULL,
    notified_at timestamp with time zone,
    created_at timestamp with time zone,
    updated_at timestamp with time zone,
    tag text DEFAULT ''::text NOT NULL
);


--
-- Name: buckets; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.buckets (
    target_ingesting_count integer NOT NULL,
    tier text DEFAULT 'small'::text NOT NULL,
    created_at text NOT NULL,
    window_duration text DEFAULT '168h'::text NOT NULL,
    label text NOT NULL,
    id bigint NOT NULL,
    state text DEFAULT 'STASIS'::text NOT NULL,
    cascade_started_at timestamp with time zone
);


--
-- Name: buckets_new_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

ALTER TABLE public.buckets ALTER COLUMN id ADD GENERATED BY DEFAULT AS IDENTITY (
    SEQUENCE NAME public.buckets_new_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1
);


--
-- Name: cron_claims; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.cron_claims (
    key text NOT NULL,
    holder text NOT NULL,
    claimed_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: cron_results; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.cron_results (
    id bigint NOT NULL,
    created_at timestamp with time zone,
    updated_at timestamp with time zone,
    deleted_at timestamp with time zone,
    cron_watch_id bigint NOT NULL,
    ran_at timestamp with time zone NOT NULL,
    stdout text,
    stderr text,
    exit_code bigint,
    duration_ms bigint,
    value double precision,
    tag text DEFAULT ''::text NOT NULL
)
PARTITION BY RANGE (ran_at);


--
-- Name: cron_results_4h; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.cron_results_4h (
    cron_watch_id bigint NOT NULL,
    tag text DEFAULT ''::text NOT NULL,
    bucket_start timestamp with time zone NOT NULL,
    count bigint NOT NULL,
    sum double precision NOT NULL,
    sum_sq double precision NOT NULL,
    min double precision NOT NULL,
    max double precision NOT NULL
)
PARTITION BY RANGE (bucket_start);


--
-- Name: cron_results_4h_default; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.cron_results_4h_default (
    cron_watch_id bigint NOT NULL,
    tag text DEFAULT ''::text NOT NULL,
    bucket_start timestamp with time zone NOT NULL,
    count bigint NOT NULL,
    sum double precision NOT NULL,
    sum_sq double precision NOT NULL,
    min double precision NOT NULL,
    max double precision NOT NULL
);


--
-- Name: cron_results_daily; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.cron_results_daily (
    cron_watch_id bigint NOT NULL,
    tag text DEFAULT ''::text NOT NULL,
    bucket_start timestamp with time zone NOT NULL,
    count bigint NOT NULL,
    sum double precision NOT NULL,
    sum_sq double precision NOT NULL,
    min double precision NOT NULL,
    max double precision NOT NULL
)
PARTITION BY RANGE (bucket_start);


--
-- Name: cron_results_hourly; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.cron_results_hourly (
    cron_watch_id bigint NOT NULL,
    tag text DEFAULT ''::text NOT NULL,
    bucket_start timestamp with time zone NOT NULL,
    count bigint NOT NULL,
    sum double precision NOT NULL,
    sum_sq double precision NOT NULL,
    min double precision NOT NULL,
    max double precision NOT NULL
)
PARTITION BY RANGE (bucket_start);


--
-- Name: cron_results_all; Type: VIEW; Schema: public; Owner: -
--

CREATE VIEW public.cron_results_all AS
 SELECT 'raw'::text AS granularity,
    cron_results.cron_watch_id,
    cron_results.tag,
    cron_results.ran_at AS bucket_start,
    (1)::bigint AS count,
    cron_results.value AS sum,
    (cron_results.value * cron_results.value) AS sum_sq,
    cron_results.value AS min,
    cron_results.value AS max
   FROM public.cron_results
  WHERE (cron_results.value IS NOT NULL)
UNION ALL
 SELECT 'hourly'::text AS granularity,
    cron_results_hourly.cron_watch_id,
    cron_results_hourly.tag,
    cron_results_hourly.bucket_start,
    cron_results_hourly.count,
    cron_results_hourly.sum,
    cron_results_hourly.sum_sq,
    cron_results_hourly.min,
    cron_results_hourly.max
   FROM public.cron_results_hourly
UNION ALL
 SELECT '4h'::text AS granularity,
    cron_results_4h.cron_watch_id,
    cron_results_4h.tag,
    cron_results_4h.bucket_start,
    cron_results_4h.count,
    cron_results_4h.sum,
    cron_results_4h.sum_sq,
    cron_results_4h.min,
    cron_results_4h.max
   FROM public.cron_results_4h
UNION ALL
 SELECT 'daily'::text AS granularity,
    cron_results_daily.cron_watch_id,
    cron_results_daily.tag,
    cron_results_daily.bucket_start,
    cron_results_daily.count,
    cron_results_daily.sum,
    cron_results_daily.sum_sq,
    cron_results_daily.min,
    cron_results_daily.max
   FROM public.cron_results_daily;


--
-- Name: cron_results_daily_default; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.cron_results_daily_default (
    cron_watch_id bigint NOT NULL,
    tag text DEFAULT ''::text NOT NULL,
    bucket_start timestamp with time zone NOT NULL,
    count bigint NOT NULL,
    sum double precision NOT NULL,
    sum_sq double precision NOT NULL,
    min double precision NOT NULL,
    max double precision NOT NULL
);


--
-- Name: cron_results_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.cron_results_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: cron_results_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.cron_results_id_seq OWNED BY public.cron_results.id;


--
-- Name: cron_results_default; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.cron_results_default (
    id bigint DEFAULT nextval('public.cron_results_id_seq'::regclass) NOT NULL,
    created_at timestamp with time zone,
    updated_at timestamp with time zone,
    deleted_at timestamp with time zone,
    cron_watch_id bigint NOT NULL,
    ran_at timestamp with time zone NOT NULL,
    stdout text,
    stderr text,
    exit_code bigint,
    duration_ms bigint,
    value double precision,
    tag text DEFAULT ''::text NOT NULL
);


--
-- Name: cron_results_hourly_default; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.cron_results_hourly_default (
    cron_watch_id bigint NOT NULL,
    tag text DEFAULT ''::text NOT NULL,
    bucket_start timestamp with time zone NOT NULL,
    count bigint NOT NULL,
    sum double precision NOT NULL,
    sum_sq double precision NOT NULL,
    min double precision NOT NULL,
    max double precision NOT NULL
);


--
-- Name: cron_watch_history; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.cron_watch_history (
    id bigint NOT NULL,
    watch_id bigint NOT NULL,
    name text NOT NULL,
    schedule text NOT NULL,
    pipeline text NOT NULL,
    content_hash text NOT NULL,
    message text DEFAULT ''::text NOT NULL,
    captured_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    captured_by_user_id text,
    authored_by_user_id text,
    predecessor_id bigint,
    org_id bigint NOT NULL,
    version_id bigint NOT NULL,
    authored_at timestamp with time zone NOT NULL,
    deleted_at timestamp with time zone
);


--
-- Name: cron_watch_history_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.cron_watch_history_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: cron_watch_history_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.cron_watch_history_id_seq OWNED BY public.cron_watch_history.id;


--
-- Name: object_version_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.object_version_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: cron_watches; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.cron_watches (
    id bigint NOT NULL,
    created_at timestamp with time zone,
    updated_at timestamp with time zone,
    deleted_at timestamp with time zone,
    name text NOT NULL,
    schedule text NOT NULL,
    pipeline text NOT NULL,
    enabled boolean DEFAULT true NOT NULL,
    last_run_at timestamp with time zone,
    last_status text,
    alert_key text,
    alert_tag text,
    alert_condition text,
    alert_threshold double precision,
    created_by_user_id text,
    content_hash text DEFAULT ''::text NOT NULL,
    message text DEFAULT ''::text NOT NULL,
    lowered_plan jsonb,
    org_id bigint NOT NULL,
    version_id bigint DEFAULT nextval('public.object_version_seq'::regclass) NOT NULL,
    authored_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    authored_by_user_id text,
    origin_version_id bigint
);


--
-- Name: cron_watches_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.cron_watches_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: cron_watches_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.cron_watches_id_seq OWNED BY public.cron_watches.id;


--
-- Name: dashboard_history; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.dashboard_history (
    id bigint NOT NULL,
    dashboard_id bigint NOT NULL,
    org_id bigint NOT NULL,
    name text NOT NULL,
    yaml_body text NOT NULL,
    message text DEFAULT ''::text NOT NULL,
    captured_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    captured_by_user_id text,
    authored_by_user_id text,
    predecessor_id bigint,
    version_id bigint NOT NULL,
    authored_at timestamp with time zone NOT NULL
);


--
-- Name: dashboard_history_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.dashboard_history_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: dashboard_history_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.dashboard_history_id_seq OWNED BY public.dashboard_history.id;


--
-- Name: dashboards; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.dashboards (
    id bigint NOT NULL,
    created_at timestamp with time zone,
    updated_at timestamp with time zone,
    deleted_at timestamp with time zone,
    name text NOT NULL,
    yaml_body text NOT NULL,
    created_by_user_id text,
    org_id bigint NOT NULL,
    version_id bigint DEFAULT nextval('public.object_version_seq'::regclass) NOT NULL,
    authored_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    authored_by_user_id text,
    origin_version_id bigint NOT NULL,
    message text DEFAULT ''::text NOT NULL
);


--
-- Name: dashboards_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.dashboards_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: dashboards_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.dashboards_id_seq OWNED BY public.dashboards.id;


--
-- Name: device_codes; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.device_codes (
    id bigint NOT NULL,
    device_code text NOT NULL,
    user_code text NOT NULL,
    user_id text,
    expires_at timestamp with time zone,
    created_at timestamp with time zone
);


--
-- Name: device_codes_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.device_codes_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: device_codes_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.device_codes_id_seq OWNED BY public.device_codes.id;


--
-- Name: goose_db_version; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.goose_db_version (
    id integer NOT NULL,
    version_id bigint NOT NULL,
    is_applied boolean NOT NULL,
    tstamp timestamp without time zone DEFAULT now() NOT NULL
);


--
-- Name: goose_db_version_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

ALTER TABLE public.goose_db_version ALTER COLUMN id ADD GENERATED BY DEFAULT AS IDENTITY (
    SEQUENCE NAME public.goose_db_version_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1
);


--
-- Name: ingestion_errors; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.ingestion_errors (
    id integer NOT NULL,
    occurred_at timestamp with time zone DEFAULT now() NOT NULL,
    shard_id text NOT NULL,
    error_msg text NOT NULL,
    row_count integer NOT NULL,
    apps text[] NOT NULL,
    customers text[] NOT NULL,
    dlq_path text,
    resolved_at timestamp with time zone
);


--
-- Name: ingestion_errors_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.ingestion_errors_id_seq
    AS integer
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: ingestion_errors_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.ingestion_errors_id_seq OWNED BY public.ingestion_errors.id;


--
-- Name: leases; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.leases (
    name text NOT NULL,
    holder text NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    acquired_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: log_group_agents; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.log_group_agents (
    log_group_id bigint NOT NULL,
    instance text NOT NULL,
    version text NOT NULL,
    seen_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: log_groups; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.log_groups (
    id bigint NOT NULL,
    created_at timestamp with time zone,
    updated_at timestamp with time zone,
    deleted_at timestamp with time zone,
    app text NOT NULL,
    log_group_name text,
    region text DEFAULT 'us-east-2'::text NOT NULL,
    schema_yaml text,
    enabled boolean DEFAULT true NOT NULL,
    source_type public.log_group_source_type NOT NULL,
    namespace_id bigint NOT NULL,
    status text DEFAULT 'configured'::text NOT NULL,
    instance text,
    deployment text,
    sample text,
    detection text,
    resample_requested_at timestamp with time zone,
    sample_collected integer,
    bucket_id bigint,
    org_id bigint NOT NULL,
    sample_updated_at timestamp with time zone,
    CONSTRAINT log_groups_status_check CHECK ((status = ANY (ARRAY['sampling'::text, 'pending'::text, 'configured'::text])))
);


--
-- Name: log_groups_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.log_groups_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: log_groups_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.log_groups_id_seq OWNED BY public.log_groups.id;


--
-- Name: magic_links; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.magic_links (
    id bigint NOT NULL,
    email text NOT NULL,
    token text NOT NULL,
    expires_at timestamp with time zone,
    used boolean DEFAULT false NOT NULL
);


--
-- Name: magic_links_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.magic_links_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: magic_links_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.magic_links_id_seq OWNED BY public.magic_links.id;


--
-- Name: memberships; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.memberships (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    email text,
    user_id text,
    role text DEFAULT 'member'::text NOT NULL,
    status text DEFAULT 'pending'::text NOT NULL,
    invited_by text,
    invited_at timestamp with time zone,
    accepted_at timestamp with time zone,
    created_at timestamp with time zone,
    updated_at timestamp with time zone,
    org_id bigint NOT NULL,
    seated boolean DEFAULT true NOT NULL,
    CONSTRAINT memberships_identified CHECK (((user_id IS NOT NULL) OR (email IS NOT NULL)))
);


--
-- Name: namespace_permissions; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.namespace_permissions (
    id bigint NOT NULL,
    created_at timestamp with time zone,
    updated_at timestamp with time zone,
    deleted_at timestamp with time zone,
    user_id text NOT NULL,
    namespace_id bigint NOT NULL,
    can_read boolean DEFAULT true NOT NULL,
    can_create_tokens boolean DEFAULT false NOT NULL
);


--
-- Name: namespace_permissions_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.namespace_permissions_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: namespace_permissions_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.namespace_permissions_id_seq OWNED BY public.namespace_permissions.id;


--
-- Name: namespaces; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.namespaces (
    id bigint NOT NULL,
    created_at timestamp with time zone,
    updated_at timestamp with time zone,
    deleted_at timestamp with time zone,
    name text NOT NULL,
    org_id bigint NOT NULL
);


--
-- Name: namespaces_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.namespaces_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: namespaces_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.namespaces_id_seq OWNED BY public.namespaces.id;


--
-- Name: notify_config_history; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.notify_config_history (
    id bigint NOT NULL,
    notify_config_id bigint NOT NULL,
    org_id bigint NOT NULL,
    aliases_yaml text NOT NULL,
    message text DEFAULT ''::text NOT NULL,
    captured_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    captured_by_user_id text,
    authored_by_user_id text,
    predecessor_id bigint,
    version_id bigint NOT NULL,
    authored_at timestamp with time zone NOT NULL
);


--
-- Name: notify_config_history_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.notify_config_history_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: notify_config_history_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.notify_config_history_id_seq OWNED BY public.notify_config_history.id;


--
-- Name: notify_configs; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.notify_configs (
    id bigint NOT NULL,
    created_at timestamp with time zone,
    updated_at timestamp with time zone,
    deleted_at timestamp with time zone,
    slack_webhook text,
    slack_bot_token text,
    slack_team_name text,
    configured_at timestamp with time zone,
    aliases_yaml text DEFAULT ''::text NOT NULL,
    org_id bigint NOT NULL,
    version_id bigint DEFAULT nextval('public.object_version_seq'::regclass) NOT NULL,
    authored_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    authored_by_user_id text,
    origin_version_id bigint NOT NULL,
    message text DEFAULT ''::text NOT NULL
);


--
-- Name: notify_configs_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.notify_configs_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: notify_configs_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.notify_configs_id_seq OWNED BY public.notify_configs.id;


--
-- Name: org_permissions; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.org_permissions (
    id bigint NOT NULL,
    created_at timestamp with time zone,
    updated_at timestamp with time zone,
    deleted_at timestamp with time zone,
    user_id text NOT NULL,
    can_manage_functions boolean DEFAULT false NOT NULL,
    can_manage_watches boolean DEFAULT false NOT NULL,
    can_manage_etc boolean DEFAULT false NOT NULL,
    org_id bigint NOT NULL,
    can_create_namespaces boolean DEFAULT false NOT NULL
);


--
-- Name: org_permissions_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.org_permissions_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: org_permissions_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.org_permissions_id_seq OWNED BY public.org_permissions.id;


--
-- Name: organizations; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.organizations (
    created_at timestamp with time zone,
    updated_at timestamp with time zone,
    deleted_at timestamp with time zone,
    name text NOT NULL,
    display_name text,
    id bigint NOT NULL,
    stripe_customer_id text,
    stripe_subscription_id text,
    seat_count integer DEFAULT 0 NOT NULL
);


--
-- Name: organizations_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

ALTER TABLE public.organizations ALTER COLUMN id ADD GENERATED BY DEFAULT AS IDENTITY (
    SEQUENCE NAME public.organizations_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1
);


--
-- Name: pagerduty_integrations; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.pagerduty_integrations (
    id bigint NOT NULL,
    created_at timestamp with time zone DEFAULT now(),
    updated_at timestamp with time zone DEFAULT now(),
    deleted_at timestamp with time zone,
    name text NOT NULL,
    integration_key text NOT NULL,
    last_used_at timestamp with time zone,
    last_error text,
    org_id bigint NOT NULL
);


--
-- Name: pagerduty_integrations_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.pagerduty_integrations_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: pagerduty_integrations_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.pagerduty_integrations_id_seq OWNED BY public.pagerduty_integrations.id;


--
-- Name: parquet_tombstones; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.parquet_tombstones (
    id bigint NOT NULL,
    path text NOT NULL,
    shard_id text NOT NULL,
    reason text NOT NULL,
    delete_after timestamp with time zone NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: parquet_tombstones_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.parquet_tombstones_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: parquet_tombstones_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.parquet_tombstones_id_seq OWNED BY public.parquet_tombstones.id;


--
-- Name: shard_archive_stats; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.shard_archive_stats (
    shard_id text NOT NULL,
    min_ts timestamp with time zone NOT NULL,
    max_ts timestamp with time zone NOT NULL,
    row_count bigint NOT NULL,
    org_id bigint NOT NULL
);


--
-- Name: shard_heartbeats; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.shard_heartbeats (
    shard_id text NOT NULL,
    instance_id text DEFAULT ''::text NOT NULL,
    addr text NOT NULL,
    state text NOT NULL,
    row_count bigint DEFAULT 0 NOT NULL,
    disk_bytes bigint DEFAULT 0 NOT NULL,
    beat_at timestamp with time zone NOT NULL
)
WITH (fillfactor='50');


--
-- Name: shard_operations; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.shard_operations (
    shard_id text NOT NULL,
    ts timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    from_state text,
    to_state text
);


--
-- Name: shard_records; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.shard_records (
    id text NOT NULL,
    window_start timestamp with time zone NOT NULL,
    window_end timestamp with time zone NOT NULL,
    state text NOT NULL,
    addr text,
    instance_id text,
    parquet_path text,
    tier text,
    row_count bigint DEFAULT 0,
    created_at timestamp with time zone NOT NULL,
    ebs_volume_id text,
    heartbeat_token text,
    bucket_id bigint NOT NULL,
    ingest_failures integer DEFAULT 0 NOT NULL,
    ingest_unhealthy boolean DEFAULT false NOT NULL,
    last_heartbeat_at timestamp with time zone,
    launch_attempts integer DEFAULT 0 NOT NULL,
    az text,
    stripe_index integer,
    binary_sha text DEFAULT ''::text NOT NULL,
    last_purged_at timestamp with time zone,
    export_snapshot_id text,
    export_instance_id text,
    export_instance_type text,
    export_started_at timestamp with time zone,
    export_attempt integer DEFAULT 0 NOT NULL,
    disk_bytes bigint DEFAULT 0 NOT NULL
);


--
-- Name: shard_services; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.shard_services (
    id bigint NOT NULL,
    shard_id text NOT NULL,
    namespace_id bigint NOT NULL,
    service text NOT NULL
);


--
-- Name: shard_services_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

ALTER TABLE public.shard_services ALTER COLUMN id ADD GENERATED BY DEFAULT AS IDENTITY (
    SEQUENCE NAME public.shard_services_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1
);


--
-- Name: shared_links; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.shared_links (
    org_id bigint NOT NULL,
    slug text NOT NULL,
    bin text NOT NULL,
    range_spec text NOT NULL,
    args text DEFAULT ''::text NOT NULL,
    created_by_user_id text,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: shell_function_history; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.shell_function_history (
    id bigint NOT NULL,
    function_id bigint NOT NULL,
    name text NOT NULL,
    source text NOT NULL,
    language text NOT NULL,
    content_hash text NOT NULL,
    message text DEFAULT ''::text NOT NULL,
    captured_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    captured_by_user_id text,
    authored_by_user_id text,
    predecessor_id bigint,
    org_id bigint NOT NULL,
    version_id bigint NOT NULL,
    authored_at timestamp with time zone NOT NULL,
    deleted_at timestamp with time zone
);


--
-- Name: shell_function_history_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.shell_function_history_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: shell_function_history_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.shell_function_history_id_seq OWNED BY public.shell_function_history.id;


--
-- Name: shell_functions; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.shell_functions (
    id bigint NOT NULL,
    created_at timestamp with time zone,
    updated_at timestamp with time zone,
    deleted_at timestamp with time zone,
    name text NOT NULL,
    source text NOT NULL,
    language text DEFAULT 'bash'::text NOT NULL,
    created_by_user_id text,
    content_hash text DEFAULT ''::text NOT NULL,
    message text DEFAULT ''::text NOT NULL,
    org_id bigint NOT NULL,
    version_id bigint DEFAULT nextval('public.object_version_seq'::regclass) NOT NULL,
    authored_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    authored_by_user_id text,
    origin_version_id bigint
);


--
-- Name: shell_functions_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.shell_functions_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: shell_functions_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.shell_functions_id_seq OWNED BY public.shell_functions.id;


--
-- Name: stripe_usage_reports; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.stripe_usage_reports (
    id bigint NOT NULL,
    day date NOT NULL,
    kb_reported bigint NOT NULL,
    stripe_event_id text NOT NULL,
    reported_at timestamp with time zone DEFAULT now() NOT NULL,
    org_id bigint NOT NULL
);


--
-- Name: stripe_usage_reports_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.stripe_usage_reports_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: stripe_usage_reports_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.stripe_usage_reports_id_seq OWNED BY public.stripe_usage_reports.id;


--
-- Name: subscriptions; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.subscriptions (
    id text NOT NULL,
    tier text NOT NULL,
    status text NOT NULL,
    starts_at timestamp with time zone NOT NULL,
    expires_at timestamp with time zone,
    stripe_subscription_id text,
    created_at timestamp with time zone,
    updated_at timestamp with time zone,
    comp_reason text DEFAULT ''::text NOT NULL,
    org_id bigint NOT NULL
);


--
-- Name: tmp_file_lines; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.tmp_file_lines (
    file_id bigint NOT NULL,
    line_num bigint NOT NULL,
    content text NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
)
PARTITION BY RANGE (updated_at);


--
-- Name: tmp_file_lines_default; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.tmp_file_lines_default (
    file_id bigint NOT NULL,
    line_num bigint NOT NULL,
    content text NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: tmp_files; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.tmp_files (
    id bigint NOT NULL,
    path text NOT NULL,
    is_dir boolean DEFAULT false NOT NULL,
    byte_size bigint DEFAULT 0 NOT NULL,
    line_count bigint DEFAULT 0 NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    created_by_user_id text,
    org_id bigint NOT NULL
)
PARTITION BY RANGE (updated_at);


--
-- Name: tmp_files_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.tmp_files_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: tmp_files_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.tmp_files_id_seq OWNED BY public.tmp_files.id;


--
-- Name: tmp_files_default; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.tmp_files_default (
    id bigint DEFAULT nextval('public.tmp_files_id_seq'::regclass) NOT NULL,
    path text NOT NULL,
    is_dir boolean DEFAULT false NOT NULL,
    byte_size bigint DEFAULT 0 NOT NULL,
    line_count bigint DEFAULT 0 NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    created_by_user_id text,
    org_id bigint NOT NULL
);


--
-- Name: usage_daily; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.usage_daily (
    day date NOT NULL,
    bytes_ingested bigint DEFAULT 0 NOT NULL,
    org_id bigint NOT NULL
)
PARTITION BY RANGE (day);


--
-- Name: usage_daily_default; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.usage_daily_default (
    day date NOT NULL,
    bytes_ingested bigint DEFAULT 0 NOT NULL,
    org_id bigint NOT NULL
);


--
-- Name: usage_hourly; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.usage_hourly (
    hour timestamp with time zone NOT NULL,
    bytes_ingested bigint DEFAULT 0 NOT NULL,
    org_id bigint NOT NULL
)
PARTITION BY RANGE (hour);


--
-- Name: usage_hourly_default; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.usage_hourly_default (
    hour timestamp with time zone NOT NULL,
    bytes_ingested bigint DEFAULT 0 NOT NULL,
    org_id bigint NOT NULL
);


--
-- Name: user_notices; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.user_notices (
    user_id text NOT NULL,
    notice_key text NOT NULL,
    kind public.notice_kind NOT NULL,
    seen_at timestamp with time zone,
    id bigint NOT NULL,
    params jsonb,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: user_notices_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.user_notices_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: user_notices_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.user_notices_id_seq OWNED BY public.user_notices.id;


--
-- Name: users; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.users (
    id text NOT NULL,
    email text NOT NULL,
    name text,
    stripe_customer_id text,
    created_at timestamp with time zone,
    updated_at timestamp with time zone,
    email_verified_at timestamp with time zone,
    has_seen_welcome boolean DEFAULT false NOT NULL,
    onboarding_step text DEFAULT 'welcome'::text NOT NULL,
    onboarding_dismissed boolean DEFAULT false NOT NULL,
    is_superadmin boolean DEFAULT false NOT NULL,
    last_active_org_id bigint
);


--
-- Name: cron_results_4h_default; Type: TABLE ATTACH; Schema: public; Owner: -
--

ALTER TABLE ONLY public.cron_results_4h ATTACH PARTITION public.cron_results_4h_default DEFAULT;


--
-- Name: cron_results_daily_default; Type: TABLE ATTACH; Schema: public; Owner: -
--

ALTER TABLE ONLY public.cron_results_daily ATTACH PARTITION public.cron_results_daily_default DEFAULT;


--
-- Name: cron_results_default; Type: TABLE ATTACH; Schema: public; Owner: -
--

ALTER TABLE ONLY public.cron_results ATTACH PARTITION public.cron_results_default DEFAULT;


--
-- Name: cron_results_hourly_default; Type: TABLE ATTACH; Schema: public; Owner: -
--

ALTER TABLE ONLY public.cron_results_hourly ATTACH PARTITION public.cron_results_hourly_default DEFAULT;


--
-- Name: tmp_file_lines_default; Type: TABLE ATTACH; Schema: public; Owner: -
--

ALTER TABLE ONLY public.tmp_file_lines ATTACH PARTITION public.tmp_file_lines_default DEFAULT;


--
-- Name: tmp_files_default; Type: TABLE ATTACH; Schema: public; Owner: -
--

ALTER TABLE ONLY public.tmp_files ATTACH PARTITION public.tmp_files_default DEFAULT;


--
-- Name: usage_daily_default; Type: TABLE ATTACH; Schema: public; Owner: -
--

ALTER TABLE ONLY public.usage_daily ATTACH PARTITION public.usage_daily_default DEFAULT;


--
-- Name: usage_hourly_default; Type: TABLE ATTACH; Schema: public; Owner: -
--

ALTER TABLE ONLY public.usage_hourly ATTACH PARTITION public.usage_hourly_default DEFAULT;


--
-- Name: agent_tokens id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.agent_tokens ALTER COLUMN id SET DEFAULT nextval('public.agent_tokens_id_seq'::regclass);


--
-- Name: cron_results id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.cron_results ALTER COLUMN id SET DEFAULT nextval('public.cron_results_id_seq'::regclass);


--
-- Name: cron_watch_history id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.cron_watch_history ALTER COLUMN id SET DEFAULT nextval('public.cron_watch_history_id_seq'::regclass);


--
-- Name: cron_watches id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.cron_watches ALTER COLUMN id SET DEFAULT nextval('public.cron_watches_id_seq'::regclass);


--
-- Name: dashboard_history id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.dashboard_history ALTER COLUMN id SET DEFAULT nextval('public.dashboard_history_id_seq'::regclass);


--
-- Name: dashboards id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.dashboards ALTER COLUMN id SET DEFAULT nextval('public.dashboards_id_seq'::regclass);


--
-- Name: device_codes id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.device_codes ALTER COLUMN id SET DEFAULT nextval('public.device_codes_id_seq'::regclass);


--
-- Name: ingestion_errors id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.ingestion_errors ALTER COLUMN id SET DEFAULT nextval('public.ingestion_errors_id_seq'::regclass);


--
-- Name: log_groups id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.log_groups ALTER COLUMN id SET DEFAULT nextval('public.log_groups_id_seq'::regclass);


--
-- Name: magic_links id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.magic_links ALTER COLUMN id SET DEFAULT nextval('public.magic_links_id_seq'::regclass);


--
-- Name: namespace_permissions id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.namespace_permissions ALTER COLUMN id SET DEFAULT nextval('public.namespace_permissions_id_seq'::regclass);


--
-- Name: namespaces id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.namespaces ALTER COLUMN id SET DEFAULT nextval('public.namespaces_id_seq'::regclass);


--
-- Name: notify_config_history id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.notify_config_history ALTER COLUMN id SET DEFAULT nextval('public.notify_config_history_id_seq'::regclass);


--
-- Name: notify_configs id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.notify_configs ALTER COLUMN id SET DEFAULT nextval('public.notify_configs_id_seq'::regclass);


--
-- Name: org_permissions id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.org_permissions ALTER COLUMN id SET DEFAULT nextval('public.org_permissions_id_seq'::regclass);


--
-- Name: pagerduty_integrations id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.pagerduty_integrations ALTER COLUMN id SET DEFAULT nextval('public.pagerduty_integrations_id_seq'::regclass);


--
-- Name: parquet_tombstones id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.parquet_tombstones ALTER COLUMN id SET DEFAULT nextval('public.parquet_tombstones_id_seq'::regclass);


--
-- Name: shell_function_history id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.shell_function_history ALTER COLUMN id SET DEFAULT nextval('public.shell_function_history_id_seq'::regclass);


--
-- Name: shell_functions id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.shell_functions ALTER COLUMN id SET DEFAULT nextval('public.shell_functions_id_seq'::regclass);


--
-- Name: stripe_usage_reports id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.stripe_usage_reports ALTER COLUMN id SET DEFAULT nextval('public.stripe_usage_reports_id_seq'::regclass);


--
-- Name: tmp_files id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.tmp_files ALTER COLUMN id SET DEFAULT nextval('public.tmp_files_id_seq'::regclass);


--
-- Name: user_notices id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.user_notices ALTER COLUMN id SET DEFAULT nextval('public.user_notices_id_seq'::regclass);


--
-- Name: agent_tokens agent_tokens_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.agent_tokens
    ADD CONSTRAINT agent_tokens_pkey PRIMARY KEY (id);


--
-- Name: alert_incidents alert_incidents_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.alert_incidents
    ADD CONSTRAINT alert_incidents_pkey PRIMARY KEY (id);


--
-- Name: buckets buckets_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.buckets
    ADD CONSTRAINT buckets_pkey PRIMARY KEY (id);


--
-- Name: cron_claims cron_claims_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.cron_claims
    ADD CONSTRAINT cron_claims_pkey PRIMARY KEY (key);


--
-- Name: cron_results_4h cron_results_4h_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.cron_results_4h
    ADD CONSTRAINT cron_results_4h_pkey PRIMARY KEY (cron_watch_id, tag, bucket_start);


--
-- Name: cron_results_4h_default cron_results_4h_default_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.cron_results_4h_default
    ADD CONSTRAINT cron_results_4h_default_pkey PRIMARY KEY (cron_watch_id, tag, bucket_start);


--
-- Name: cron_results_daily cron_results_daily_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.cron_results_daily
    ADD CONSTRAINT cron_results_daily_pkey PRIMARY KEY (cron_watch_id, tag, bucket_start);


--
-- Name: cron_results_daily_default cron_results_daily_default_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.cron_results_daily_default
    ADD CONSTRAINT cron_results_daily_default_pkey PRIMARY KEY (cron_watch_id, tag, bucket_start);


--
-- Name: cron_results cron_results_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.cron_results
    ADD CONSTRAINT cron_results_pkey PRIMARY KEY (id, ran_at);


--
-- Name: cron_results_default cron_results_default_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.cron_results_default
    ADD CONSTRAINT cron_results_default_pkey PRIMARY KEY (id, ran_at);


--
-- Name: cron_results_hourly cron_results_hourly_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.cron_results_hourly
    ADD CONSTRAINT cron_results_hourly_pkey PRIMARY KEY (cron_watch_id, tag, bucket_start);


--
-- Name: cron_results_hourly_default cron_results_hourly_default_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.cron_results_hourly_default
    ADD CONSTRAINT cron_results_hourly_default_pkey PRIMARY KEY (cron_watch_id, tag, bucket_start);


--
-- Name: cron_watch_history cron_watch_history_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.cron_watch_history
    ADD CONSTRAINT cron_watch_history_pkey PRIMARY KEY (id);


--
-- Name: cron_watches cron_watches_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.cron_watches
    ADD CONSTRAINT cron_watches_pkey PRIMARY KEY (id);


--
-- Name: organizations customers_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.organizations
    ADD CONSTRAINT customers_pkey PRIMARY KEY (id);


--
-- Name: dashboard_history dashboard_history_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.dashboard_history
    ADD CONSTRAINT dashboard_history_pkey PRIMARY KEY (id);


--
-- Name: dashboards dashboards_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.dashboards
    ADD CONSTRAINT dashboards_pkey PRIMARY KEY (id);


--
-- Name: device_codes device_codes_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.device_codes
    ADD CONSTRAINT device_codes_pkey PRIMARY KEY (id);


--
-- Name: goose_db_version goose_db_version_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.goose_db_version
    ADD CONSTRAINT goose_db_version_pkey PRIMARY KEY (id);


--
-- Name: ingestion_errors ingestion_errors_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.ingestion_errors
    ADD CONSTRAINT ingestion_errors_pkey PRIMARY KEY (id);


--
-- Name: leases leases_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.leases
    ADD CONSTRAINT leases_pkey PRIMARY KEY (name);


--
-- Name: log_group_agents log_group_agents_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.log_group_agents
    ADD CONSTRAINT log_group_agents_pkey PRIMARY KEY (log_group_id, instance);


--
-- Name: log_groups log_groups_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.log_groups
    ADD CONSTRAINT log_groups_pkey PRIMARY KEY (id);


--
-- Name: magic_links magic_links_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.magic_links
    ADD CONSTRAINT magic_links_pkey PRIMARY KEY (id);


--
-- Name: namespace_permissions namespace_permissions_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.namespace_permissions
    ADD CONSTRAINT namespace_permissions_pkey PRIMARY KEY (id);


--
-- Name: namespaces namespaces_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.namespaces
    ADD CONSTRAINT namespaces_pkey PRIMARY KEY (id);


--
-- Name: notify_config_history notify_config_history_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.notify_config_history
    ADD CONSTRAINT notify_config_history_pkey PRIMARY KEY (id);


--
-- Name: notify_configs notify_configs_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.notify_configs
    ADD CONSTRAINT notify_configs_pkey PRIMARY KEY (id);


--
-- Name: org_permissions org_permissions_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.org_permissions
    ADD CONSTRAINT org_permissions_pkey PRIMARY KEY (id);


--
-- Name: pagerduty_integrations pagerduty_integrations_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.pagerduty_integrations
    ADD CONSTRAINT pagerduty_integrations_pkey PRIMARY KEY (id);


--
-- Name: parquet_tombstones parquet_tombstones_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.parquet_tombstones
    ADD CONSTRAINT parquet_tombstones_pkey PRIMARY KEY (id);


--
-- Name: shard_archive_stats shard_archive_stats_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.shard_archive_stats
    ADD CONSTRAINT shard_archive_stats_pkey PRIMARY KEY (shard_id, org_id);


--
-- Name: shard_heartbeats shard_heartbeats_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.shard_heartbeats
    ADD CONSTRAINT shard_heartbeats_pkey PRIMARY KEY (shard_id, instance_id);


--
-- Name: shard_records shard_records_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.shard_records
    ADD CONSTRAINT shard_records_pkey PRIMARY KEY (id);


--
-- Name: shard_services shard_services_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.shard_services
    ADD CONSTRAINT shard_services_pkey PRIMARY KEY (id);


--
-- Name: shard_services shard_services_shard_id_namespace_id_service_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.shard_services
    ADD CONSTRAINT shard_services_shard_id_namespace_id_service_key UNIQUE (shard_id, namespace_id, service);


--
-- Name: shared_links shared_links_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.shared_links
    ADD CONSTRAINT shared_links_pkey PRIMARY KEY (org_id, slug);


--
-- Name: shell_function_history shell_function_history_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.shell_function_history
    ADD CONSTRAINT shell_function_history_pkey PRIMARY KEY (id);


--
-- Name: shell_functions shell_functions_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.shell_functions
    ADD CONSTRAINT shell_functions_pkey PRIMARY KEY (id);


--
-- Name: stripe_usage_reports stripe_usage_reports_org_id_day_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.stripe_usage_reports
    ADD CONSTRAINT stripe_usage_reports_org_id_day_key UNIQUE (org_id, day);


--
-- Name: stripe_usage_reports stripe_usage_reports_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.stripe_usage_reports
    ADD CONSTRAINT stripe_usage_reports_pkey PRIMARY KEY (id);


--
-- Name: subscriptions subscriptions_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.subscriptions
    ADD CONSTRAINT subscriptions_pkey PRIMARY KEY (id);


--
-- Name: memberships team_members_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.memberships
    ADD CONSTRAINT team_members_pkey PRIMARY KEY (id);


--
-- Name: tmp_file_lines tmp_file_lines_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.tmp_file_lines
    ADD CONSTRAINT tmp_file_lines_pkey PRIMARY KEY (file_id, line_num, updated_at);


--
-- Name: tmp_file_lines_default tmp_file_lines_default_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.tmp_file_lines_default
    ADD CONSTRAINT tmp_file_lines_default_pkey PRIMARY KEY (file_id, line_num, updated_at);


--
-- Name: tmp_files tmp_files_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.tmp_files
    ADD CONSTRAINT tmp_files_pkey PRIMARY KEY (id, updated_at);


--
-- Name: tmp_files_default tmp_files_default_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.tmp_files_default
    ADD CONSTRAINT tmp_files_default_pkey PRIMARY KEY (id, updated_at);


--
-- Name: usage_daily usage_daily_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.usage_daily
    ADD CONSTRAINT usage_daily_pkey PRIMARY KEY (org_id, day);


--
-- Name: usage_daily_default usage_daily_default_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.usage_daily_default
    ADD CONSTRAINT usage_daily_default_pkey PRIMARY KEY (org_id, day);


--
-- Name: user_notices user_notices_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.user_notices
    ADD CONSTRAINT user_notices_pkey PRIMARY KEY (id);


--
-- Name: users users_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.users
    ADD CONSTRAINT users_pkey PRIMARY KEY (id);


--
-- Name: agent_tokens_public_id_key; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX agent_tokens_public_id_key ON public.agent_tokens USING btree (public_id);


--
-- Name: buckets_label_key; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX buckets_label_key ON public.buckets USING btree (label);


--
-- Name: cron_claims_claimed_at_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX cron_claims_claimed_at_idx ON public.cron_claims USING btree (claimed_at);


--
-- Name: idx_cron_4h_watch_tag_bucket; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_cron_4h_watch_tag_bucket ON ONLY public.cron_results_4h USING btree (cron_watch_id, tag, bucket_start DESC);


--
-- Name: cron_results_4h_default_cron_watch_id_tag_bucket_start_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX cron_results_4h_default_cron_watch_id_tag_bucket_start_idx ON public.cron_results_4h_default USING btree (cron_watch_id, tag, bucket_start DESC);


--
-- Name: idx_cron_daily_watch_tag_bucket; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_cron_daily_watch_tag_bucket ON ONLY public.cron_results_daily USING btree (cron_watch_id, tag, bucket_start DESC);


--
-- Name: cron_results_daily_default_cron_watch_id_tag_bucket_start_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX cron_results_daily_default_cron_watch_id_tag_bucket_start_idx ON public.cron_results_daily_default USING btree (cron_watch_id, tag, bucket_start DESC);


--
-- Name: idx_cron_results_watch_tag_ran_at; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_cron_results_watch_tag_ran_at ON ONLY public.cron_results USING btree (cron_watch_id, tag, ran_at DESC);


--
-- Name: cron_results_default_cron_watch_id_tag_ran_at_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX cron_results_default_cron_watch_id_tag_ran_at_idx ON public.cron_results_default USING btree (cron_watch_id, tag, ran_at DESC);


--
-- Name: idx_cron_results_deleted_at; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_cron_results_deleted_at ON ONLY public.cron_results USING btree (deleted_at);


--
-- Name: cron_results_default_deleted_at_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX cron_results_default_deleted_at_idx ON public.cron_results_default USING btree (deleted_at);


--
-- Name: idx_cron_hourly_watch_tag_bucket; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_cron_hourly_watch_tag_bucket ON ONLY public.cron_results_hourly USING btree (cron_watch_id, tag, bucket_start DESC);


--
-- Name: cron_results_hourly_default_cron_watch_id_tag_bucket_start_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX cron_results_hourly_default_cron_watch_id_tag_bucket_start_idx ON public.cron_results_hourly_default USING btree (cron_watch_id, tag, bucket_start DESC);


--
-- Name: idx_agent_tokens_deleted_at; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_agent_tokens_deleted_at ON public.agent_tokens USING btree (deleted_at);


--
-- Name: idx_agent_tokens_token_hash; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX idx_agent_tokens_token_hash ON public.agent_tokens USING btree (token_hash);


--
-- Name: idx_agent_tokens_version_seen; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_agent_tokens_version_seen ON public.agent_tokens USING btree (last_seen_version, last_seen_at);


--
-- Name: idx_cron_org_name; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX idx_cron_org_name ON public.cron_watches USING btree (org_id, name);


--
-- Name: idx_cron_watch_history_version_id; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX idx_cron_watch_history_version_id ON public.cron_watch_history USING btree (version_id);


--
-- Name: idx_cron_watches_deleted_at; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_cron_watches_deleted_at ON public.cron_watches USING btree (deleted_at);


--
-- Name: idx_cron_watches_version_id; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX idx_cron_watches_version_id ON public.cron_watches USING btree (version_id);


--
-- Name: idx_customers_deleted_at; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_customers_deleted_at ON public.organizations USING btree (deleted_at);


--
-- Name: idx_customers_name; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX idx_customers_name ON public.organizations USING btree (name);


--
-- Name: idx_cwh_content_hash; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_cwh_content_hash ON public.cron_watch_history USING btree (watch_id, content_hash);


--
-- Name: idx_cwh_org_name; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_cwh_org_name ON public.cron_watch_history USING btree (org_id, name, captured_at DESC);


--
-- Name: idx_cwh_watch_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_cwh_watch_id ON public.cron_watch_history USING btree (watch_id, captured_at DESC);


--
-- Name: idx_dashboards_deleted_at; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_dashboards_deleted_at ON public.dashboards USING btree (deleted_at);


--
-- Name: idx_dashboards_org_name; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX idx_dashboards_org_name ON public.dashboards USING btree (org_id, name) WHERE (deleted_at IS NULL);


--
-- Name: idx_dashboards_version_id; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX idx_dashboards_version_id ON public.dashboards USING btree (version_id);


--
-- Name: idx_device_codes_device_code; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX idx_device_codes_device_code ON public.device_codes USING btree (device_code);


--
-- Name: idx_device_codes_user_code; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX idx_device_codes_user_code ON public.device_codes USING btree (user_code);


--
-- Name: idx_dh_dashboard_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_dh_dashboard_id ON public.dashboard_history USING btree (dashboard_id, captured_at DESC);


--
-- Name: idx_dh_version_id; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX idx_dh_version_id ON public.dashboard_history USING btree (version_id);


--
-- Name: idx_func_org_name; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX idx_func_org_name ON public.shell_functions USING btree (org_id, name);


--
-- Name: idx_incident_watch_condition_tag_open; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX idx_incident_watch_condition_tag_open ON public.alert_incidents USING btree (cron_watch_id, condition, tag) WHERE (closed_at IS NULL);


--
-- Name: idx_ingestion_errors_unresolved; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_ingestion_errors_unresolved ON public.ingestion_errors USING btree (occurred_at) WHERE (resolved_at IS NULL);


--
-- Name: idx_log_groups_deleted_at; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_log_groups_deleted_at ON public.log_groups USING btree (deleted_at);


--
-- Name: idx_log_groups_org; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_log_groups_org ON public.log_groups USING btree (org_id);


--
-- Name: idx_magic_links_token; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX idx_magic_links_token ON public.magic_links USING btree (token);


--
-- Name: idx_memberships_org; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_memberships_org ON public.memberships USING btree (org_id);


--
-- Name: idx_memberships_org_email; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX idx_memberships_org_email ON public.memberships USING btree (org_id, lower(email)) WHERE (email IS NOT NULL);


--
-- Name: idx_memberships_org_user; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX idx_memberships_org_user ON public.memberships USING btree (org_id, user_id) WHERE (user_id IS NOT NULL);


--
-- Name: idx_memberships_user; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_memberships_user ON public.memberships USING btree (user_id);


--
-- Name: idx_namespace_permissions_deleted_at; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_namespace_permissions_deleted_at ON public.namespace_permissions USING btree (deleted_at);


--
-- Name: idx_namespaces_deleted_at; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_namespaces_deleted_at ON public.namespaces USING btree (deleted_at);


--
-- Name: idx_nch_config_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_nch_config_id ON public.notify_config_history USING btree (notify_config_id, captured_at DESC);


--
-- Name: idx_nch_version_id; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX idx_nch_version_id ON public.notify_config_history USING btree (version_id);


--
-- Name: idx_notify_configs_deleted_at; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_notify_configs_deleted_at ON public.notify_configs USING btree (deleted_at);


--
-- Name: idx_notify_configs_org_id; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX idx_notify_configs_org_id ON public.notify_configs USING btree (org_id);


--
-- Name: idx_notify_configs_version_id; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX idx_notify_configs_version_id ON public.notify_configs USING btree (version_id);


--
-- Name: idx_ns_app; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX idx_ns_app ON public.log_groups USING btree (namespace_id, app);


--
-- Name: idx_ns_org_name; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX idx_ns_org_name ON public.namespaces USING btree (org_id, name);


--
-- Name: idx_nsperm_user_ns; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX idx_nsperm_user_ns ON public.namespace_permissions USING btree (user_id, namespace_id);


--
-- Name: idx_org_permissions_deleted_at; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_org_permissions_deleted_at ON public.org_permissions USING btree (deleted_at);


--
-- Name: idx_orgperm_org_user; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX idx_orgperm_org_user ON public.org_permissions USING btree (org_id, user_id);


--
-- Name: idx_pd_integrations_deleted_at; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_pd_integrations_deleted_at ON public.pagerduty_integrations USING btree (deleted_at);


--
-- Name: idx_pd_integrations_org_name; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX idx_pd_integrations_org_name ON public.pagerduty_integrations USING btree (org_id, name) WHERE (deleted_at IS NULL);


--
-- Name: idx_sfh_content_hash; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_sfh_content_hash ON public.shell_function_history USING btree (function_id, content_hash);


--
-- Name: idx_sfh_function_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_sfh_function_id ON public.shell_function_history USING btree (function_id, captured_at DESC);


--
-- Name: idx_sfh_org_name; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_sfh_org_name ON public.shell_function_history USING btree (org_id, name, captured_at DESC);


--
-- Name: idx_shard_operations_shard_ts; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_shard_operations_shard_ts ON public.shard_operations USING btree (shard_id, ts);


--
-- Name: idx_shard_operations_ts; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_shard_operations_ts ON public.shard_operations USING btree (ts);


--
-- Name: idx_shard_records_bucket_window; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_shard_records_bucket_window ON public.shard_records USING btree (bucket_id, window_start);


--
-- Name: idx_shard_services_ns_service; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_shard_services_ns_service ON public.shard_services USING btree (namespace_id, service);


--
-- Name: idx_shell_function_history_version_id; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX idx_shell_function_history_version_id ON public.shell_function_history USING btree (version_id);


--
-- Name: idx_shell_functions_deleted_at; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_shell_functions_deleted_at ON public.shell_functions USING btree (deleted_at);


--
-- Name: idx_shell_functions_version_id; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX idx_shell_functions_version_id ON public.shell_functions USING btree (version_id);


--
-- Name: idx_stripe_usage_reports_day; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_stripe_usage_reports_day ON public.stripe_usage_reports USING btree (day);


--
-- Name: idx_subscriptions_org; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_subscriptions_org ON public.subscriptions USING btree (org_id);


--
-- Name: idx_tmp_file_lines_file_line; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_tmp_file_lines_file_line ON ONLY public.tmp_file_lines USING btree (file_id, line_num);


--
-- Name: idx_tmp_files_org_path; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_tmp_files_org_path ON ONLY public.tmp_files USING btree (org_id, path);


--
-- Name: idx_usage_hourly_customer_hour; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_usage_hourly_customer_hour ON ONLY public.usage_hourly USING btree (org_id, hour);


--
-- Name: idx_usage_hourly_hour_brin; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_usage_hourly_hour_brin ON ONLY public.usage_hourly USING brin (hour);


--
-- Name: idx_user_notices_pending; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_user_notices_pending ON public.user_notices USING btree (user_id) WHERE (seen_at IS NULL);


--
-- Name: idx_users_email; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX idx_users_email ON public.users USING btree (email);


--
-- Name: parquet_tombstones_delete_after_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX parquet_tombstones_delete_after_idx ON public.parquet_tombstones USING btree (delete_after);


--
-- Name: shard_archive_stats_org_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX shard_archive_stats_org_idx ON public.shard_archive_stats USING btree (org_id);


--
-- Name: tmp_file_lines_default_file_id_line_num_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX tmp_file_lines_default_file_id_line_num_idx ON public.tmp_file_lines_default USING btree (file_id, line_num);


--
-- Name: tmp_files_default_org_id_path_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX tmp_files_default_org_id_path_idx ON public.tmp_files_default USING btree (org_id, path);


--
-- Name: usage_hourly_default_hour_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX usage_hourly_default_hour_idx ON public.usage_hourly_default USING brin (hour);


--
-- Name: usage_hourly_default_org_id_hour_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX usage_hourly_default_org_id_hour_idx ON public.usage_hourly_default USING btree (org_id, hour);


--
-- Name: cron_results_4h_default_cron_watch_id_tag_bucket_start_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.idx_cron_4h_watch_tag_bucket ATTACH PARTITION public.cron_results_4h_default_cron_watch_id_tag_bucket_start_idx;


--
-- Name: cron_results_4h_default_pkey; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.cron_results_4h_pkey ATTACH PARTITION public.cron_results_4h_default_pkey;


--
-- Name: cron_results_daily_default_cron_watch_id_tag_bucket_start_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.idx_cron_daily_watch_tag_bucket ATTACH PARTITION public.cron_results_daily_default_cron_watch_id_tag_bucket_start_idx;


--
-- Name: cron_results_daily_default_pkey; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.cron_results_daily_pkey ATTACH PARTITION public.cron_results_daily_default_pkey;


--
-- Name: cron_results_default_cron_watch_id_tag_ran_at_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.idx_cron_results_watch_tag_ran_at ATTACH PARTITION public.cron_results_default_cron_watch_id_tag_ran_at_idx;


--
-- Name: cron_results_default_deleted_at_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.idx_cron_results_deleted_at ATTACH PARTITION public.cron_results_default_deleted_at_idx;


--
-- Name: cron_results_default_pkey; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.cron_results_pkey ATTACH PARTITION public.cron_results_default_pkey;


--
-- Name: cron_results_hourly_default_cron_watch_id_tag_bucket_start_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.idx_cron_hourly_watch_tag_bucket ATTACH PARTITION public.cron_results_hourly_default_cron_watch_id_tag_bucket_start_idx;


--
-- Name: cron_results_hourly_default_pkey; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.cron_results_hourly_pkey ATTACH PARTITION public.cron_results_hourly_default_pkey;


--
-- Name: tmp_file_lines_default_file_id_line_num_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.idx_tmp_file_lines_file_line ATTACH PARTITION public.tmp_file_lines_default_file_id_line_num_idx;


--
-- Name: tmp_file_lines_default_pkey; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.tmp_file_lines_pkey ATTACH PARTITION public.tmp_file_lines_default_pkey;


--
-- Name: tmp_files_default_org_id_path_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.idx_tmp_files_org_path ATTACH PARTITION public.tmp_files_default_org_id_path_idx;


--
-- Name: tmp_files_default_pkey; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.tmp_files_pkey ATTACH PARTITION public.tmp_files_default_pkey;


--
-- Name: usage_daily_default_pkey; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.usage_daily_pkey ATTACH PARTITION public.usage_daily_default_pkey;


--
-- Name: usage_hourly_default_hour_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.idx_usage_hourly_hour_brin ATTACH PARTITION public.usage_hourly_default_hour_idx;


--
-- Name: usage_hourly_default_org_id_hour_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.idx_usage_hourly_customer_hour ATTACH PARTITION public.usage_hourly_default_org_id_hour_idx;


--
-- Name: cron_watches capture_cron_watch_history_trg; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER capture_cron_watch_history_trg BEFORE UPDATE OF schedule, pipeline, deleted_at ON public.cron_watches FOR EACH ROW EXECUTE FUNCTION public.capture_cron_watch_history();


--
-- Name: dashboards capture_dashboard_history_trg; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER capture_dashboard_history_trg BEFORE UPDATE OF yaml_body ON public.dashboards FOR EACH ROW EXECUTE FUNCTION public.capture_dashboard_history();


--
-- Name: notify_configs capture_notify_config_history_trg; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER capture_notify_config_history_trg BEFORE UPDATE OF aliases_yaml ON public.notify_configs FOR EACH ROW EXECUTE FUNCTION public.capture_notify_config_history();


--
-- Name: shell_functions capture_shell_function_history_trg; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER capture_shell_function_history_trg BEFORE UPDATE OF source, language, deleted_at ON public.shell_functions FOR EACH ROW EXECUTE FUNCTION public.capture_shell_function_history();


--
-- Name: cron_watches cron_watches_authored_by_trg; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER cron_watches_authored_by_trg BEFORE INSERT ON public.cron_watches FOR EACH ROW EXECUTE FUNCTION public.set_authored_by_on_insert();


--
-- Name: dashboards dashboards_origin_trg; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER dashboards_origin_trg BEFORE INSERT ON public.dashboards FOR EACH ROW EXECUTE FUNCTION public.etc_set_origin_on_insert();


--
-- Name: log_groups log_groups_fill_org_id_bi; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER log_groups_fill_org_id_bi BEFORE INSERT OR UPDATE ON public.log_groups FOR EACH ROW EXECUTE FUNCTION public.log_groups_fill_org_id();


--
-- Name: notify_configs notify_configs_origin_trg; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER notify_configs_origin_trg BEFORE INSERT ON public.notify_configs FOR EACH ROW EXECUTE FUNCTION public.etc_set_origin_on_insert();


--
-- Name: shard_records shard_records_audit_row; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER shard_records_audit_row AFTER INSERT OR DELETE ON public.shard_records FOR EACH ROW EXECUTE FUNCTION public.log_shard_operation();


--
-- Name: shard_records shard_records_audit_state; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER shard_records_audit_state AFTER UPDATE ON public.shard_records FOR EACH ROW WHEN ((old.state IS DISTINCT FROM new.state)) EXECUTE FUNCTION public.log_shard_operation();


--
-- Name: shell_functions shell_functions_authored_by_trg; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER shell_functions_authored_by_trg BEFORE INSERT ON public.shell_functions FOR EACH ROW EXECUTE FUNCTION public.set_authored_by_on_insert();


--
-- Name: memberships sync_membership_org_permissions; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER sync_membership_org_permissions AFTER INSERT OR UPDATE OF role, status ON public.memberships FOR EACH ROW EXECUTE FUNCTION public.sync_membership_org_permissions();


--
-- Name: cron_watch_history cron_watch_history_predecessor_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.cron_watch_history
    ADD CONSTRAINT cron_watch_history_predecessor_id_fkey FOREIGN KEY (predecessor_id) REFERENCES public.cron_watch_history(id) ON DELETE SET NULL;


--
-- Name: dashboard_history dashboard_history_predecessor_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.dashboard_history
    ADD CONSTRAINT dashboard_history_predecessor_id_fkey FOREIGN KEY (predecessor_id) REFERENCES public.dashboard_history(id) ON DELETE SET NULL;


--
-- Name: agent_tokens fk_agent_tokens_namespace; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.agent_tokens
    ADD CONSTRAINT fk_agent_tokens_namespace FOREIGN KEY (namespace_id) REFERENCES public.namespaces(id) ON DELETE CASCADE;


--
-- Name: log_groups fk_log_groups_org; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.log_groups
    ADD CONSTRAINT fk_log_groups_org FOREIGN KEY (org_id) REFERENCES public.organizations(id) ON DELETE CASCADE;


--
-- Name: agent_tokens fk_organizations_agent_tokens; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.agent_tokens
    ADD CONSTRAINT fk_organizations_agent_tokens FOREIGN KEY (org_id) REFERENCES public.organizations(id) ON DELETE CASCADE;


--
-- Name: namespaces fk_organizations_namespaces; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.namespaces
    ADD CONSTRAINT fk_organizations_namespaces FOREIGN KEY (org_id) REFERENCES public.organizations(id) ON DELETE CASCADE;


--
-- Name: pagerduty_integrations fk_pd_integrations_org; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.pagerduty_integrations
    ADD CONSTRAINT fk_pd_integrations_org FOREIGN KEY (org_id) REFERENCES public.organizations(id) ON DELETE CASCADE;


--
-- Name: subscriptions fk_subscriptions_org; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.subscriptions
    ADD CONSTRAINT fk_subscriptions_org FOREIGN KEY (org_id) REFERENCES public.organizations(id) ON DELETE CASCADE;


--
-- Name: memberships fk_team_members_org; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.memberships
    ADD CONSTRAINT fk_team_members_org FOREIGN KEY (org_id) REFERENCES public.organizations(id) ON DELETE CASCADE;


--
-- Name: log_group_agents log_group_agents_log_group_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.log_group_agents
    ADD CONSTRAINT log_group_agents_log_group_id_fkey FOREIGN KEY (log_group_id) REFERENCES public.log_groups(id) ON DELETE CASCADE;


--
-- Name: log_groups log_groups_bucket_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.log_groups
    ADD CONSTRAINT log_groups_bucket_id_fkey FOREIGN KEY (bucket_id) REFERENCES public.buckets(id);


--
-- Name: log_groups log_groups_namespace_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.log_groups
    ADD CONSTRAINT log_groups_namespace_id_fkey FOREIGN KEY (namespace_id) REFERENCES public.namespaces(id) ON DELETE CASCADE;


--
-- Name: notify_config_history notify_config_history_predecessor_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.notify_config_history
    ADD CONSTRAINT notify_config_history_predecessor_id_fkey FOREIGN KEY (predecessor_id) REFERENCES public.notify_config_history(id) ON DELETE SET NULL;


--
-- Name: shard_archive_stats shard_archive_stats_shard_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.shard_archive_stats
    ADD CONSTRAINT shard_archive_stats_shard_id_fkey FOREIGN KEY (shard_id) REFERENCES public.shard_records(id) ON DELETE CASCADE;


--
-- Name: shard_records shard_records_bucket_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.shard_records
    ADD CONSTRAINT shard_records_bucket_id_fkey FOREIGN KEY (bucket_id) REFERENCES public.buckets(id);


--
-- Name: shard_services shard_services_namespace_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.shard_services
    ADD CONSTRAINT shard_services_namespace_id_fkey FOREIGN KEY (namespace_id) REFERENCES public.namespaces(id) ON DELETE CASCADE;


--
-- Name: shard_services shard_services_shard_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.shard_services
    ADD CONSTRAINT shard_services_shard_id_fkey FOREIGN KEY (shard_id) REFERENCES public.shard_records(id) ON DELETE CASCADE;


--
-- Name: shell_function_history shell_function_history_predecessor_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.shell_function_history
    ADD CONSTRAINT shell_function_history_predecessor_id_fkey FOREIGN KEY (predecessor_id) REFERENCES public.shell_function_history(id) ON DELETE SET NULL;


--
-- Name: user_notices user_notices_user_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.user_notices
    ADD CONSTRAINT user_notices_user_id_fkey FOREIGN KEY (user_id) REFERENCES public.users(id) ON DELETE CASCADE;


--
-- Name: users users_last_active_org_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.users
    ADD CONSTRAINT users_last_active_org_id_fkey FOREIGN KEY (last_active_org_id) REFERENCES public.organizations(id) ON DELETE SET NULL;


--
-- PostgreSQL database dump complete
--

\unrestrict xcqB3V4xpJgLH7UDHdnpVyG3qp0zTmBo6LFUTmNd2DfWLF7JUBoReX7jBx7qwSa

--
-- PostgreSQL database dump
--

\restrict WMs3f7dc9DK7XfQ0ozYC32tKiw5SpEv2SML5UWaJ3tbe16gwCVE6cuqBFYMgSBx

-- Dumped from database version 16.15 (Debian 16.15-1.pgdg13+2)
-- Dumped by pg_dump version 16.15 (Debian 16.15-1.pgdg13+2)

SET statement_timeout = 0;
SET lock_timeout = 0;
SET idle_in_transaction_session_timeout = 0;
SET client_encoding = 'UTF8';
SET standard_conforming_strings = on;
SELECT pg_catalog.set_config('search_path', '', false);
SET check_function_bodies = false;
SET xmloption = content;
SET client_min_messages = warning;
SET row_security = off;

--
-- Data for Name: buckets; Type: TABLE DATA; Schema: public; Owner: -
--

INSERT INTO public.buckets VALUES (2, 'small', '2026-05-06T04:47:55Z', '168h', 'default', 1, 'STASIS', NULL);


--
-- Name: buckets_new_id_seq; Type: SEQUENCE SET; Schema: public; Owner: -
--

SELECT pg_catalog.setval('public.buckets_new_id_seq', 1, true);


--
-- PostgreSQL database dump complete
--

\unrestrict WMs3f7dc9DK7XfQ0ozYC32tKiw5SpEv2SML5UWaJ3tbe16gwCVE6cuqBFYMgSBx

--
-- PostgreSQL database dump
--

\restrict RD3iG3nioPArVAq5JwT8jrsJCe6eycq6eKQc2rj6Cy0OyDzEnthPLpybRHbr4BK

-- Dumped from database version 16.15 (Debian 16.15-1.pgdg13+2)
-- Dumped by pg_dump version 16.15 (Debian 16.15-1.pgdg13+2)

SET statement_timeout = 0;
SET lock_timeout = 0;
SET idle_in_transaction_session_timeout = 0;
SET client_encoding = 'UTF8';
SET standard_conforming_strings = on;
SELECT pg_catalog.set_config('search_path', '', false);
SET check_function_bodies = false;
SET xmloption = content;
SET client_min_messages = warning;
SET row_security = off;

--
-- Data for Name: goose_db_version; Type: TABLE DATA; Schema: public; Owner: -
--

INSERT INTO public.goose_db_version VALUES (1, 0, true, '2026-05-03 02:48:56.197392');
INSERT INTO public.goose_db_version VALUES (2, 1, true, '2026-05-03 02:48:56.213304');
INSERT INTO public.goose_db_version VALUES (3, 2, true, '2026-05-03 02:48:56.391041');
INSERT INTO public.goose_db_version VALUES (4, 3, true, '2026-05-03 02:48:56.580747');
INSERT INTO public.goose_db_version VALUES (5, 4, true, '2026-05-03 02:48:56.621869');
INSERT INTO public.goose_db_version VALUES (6, 5, true, '2026-05-03 02:48:56.624499');
INSERT INTO public.goose_db_version VALUES (7, 6, true, '2026-05-03 02:48:56.637333');
INSERT INTO public.goose_db_version VALUES (8, 7, true, '2026-05-03 02:48:56.643058');
INSERT INTO public.goose_db_version VALUES (9, 8, true, '2026-05-03 02:48:56.650192');
INSERT INTO public.goose_db_version VALUES (10, 9, true, '2026-05-03 02:48:56.652229');
INSERT INTO public.goose_db_version VALUES (11, 10, true, '2026-05-03 02:48:56.654286');
INSERT INTO public.goose_db_version VALUES (12, 11, true, '2026-05-03 02:48:56.693133');
INSERT INTO public.goose_db_version VALUES (13, 12, true, '2026-05-03 02:48:56.697773');
INSERT INTO public.goose_db_version VALUES (14, 13, true, '2026-05-03 02:48:56.706497');
INSERT INTO public.goose_db_version VALUES (15, 14, true, '2026-05-03 02:48:56.708904');
INSERT INTO public.goose_db_version VALUES (16, 15, true, '2026-05-03 02:48:56.711598');
INSERT INTO public.goose_db_version VALUES (17, 16, true, '2026-05-03 02:48:56.714513');
INSERT INTO public.goose_db_version VALUES (18, 17, true, '2026-05-03 02:48:56.716498');
INSERT INTO public.goose_db_version VALUES (19, 18, true, '2026-05-03 02:48:56.721645');
INSERT INTO public.goose_db_version VALUES (20, 19, true, '2026-05-03 02:48:56.726603');
INSERT INTO public.goose_db_version VALUES (21, 20, true, '2026-05-03 02:48:56.731194');
INSERT INTO public.goose_db_version VALUES (22, 21, true, '2026-05-03 02:48:56.746969');
INSERT INTO public.goose_db_version VALUES (23, 22, true, '2026-05-03 02:48:56.75403');
INSERT INTO public.goose_db_version VALUES (24, 23, true, '2026-05-03 02:48:56.776104');
INSERT INTO public.goose_db_version VALUES (25, 24, true, '2026-05-03 02:48:56.88155');
INSERT INTO public.goose_db_version VALUES (26, 25, true, '2026-05-03 02:48:56.908672');
INSERT INTO public.goose_db_version VALUES (27, 26, true, '2026-05-03 02:48:56.911865');
INSERT INTO public.goose_db_version VALUES (28, 27, true, '2026-05-03 02:48:57.125742');
INSERT INTO public.goose_db_version VALUES (29, 28, true, '2026-05-03 02:48:57.128107');
INSERT INTO public.goose_db_version VALUES (30, 29, true, '2026-05-03 02:48:57.131458');
INSERT INTO public.goose_db_version VALUES (31, 30, true, '2026-05-03 02:48:57.14214');
INSERT INTO public.goose_db_version VALUES (32, 31, true, '2026-05-03 02:48:57.316198');
INSERT INTO public.goose_db_version VALUES (33, 32, true, '2026-05-03 02:48:57.319267');
INSERT INTO public.goose_db_version VALUES (34, 33, true, '2026-05-03 02:48:57.322689');
INSERT INTO public.goose_db_version VALUES (35, 34, true, '2026-05-03 02:48:57.328106');
INSERT INTO public.goose_db_version VALUES (36, 35, true, '2026-05-03 02:48:57.331306');
INSERT INTO public.goose_db_version VALUES (37, 36, true, '2026-05-03 02:48:57.34053');
INSERT INTO public.goose_db_version VALUES (38, 37, true, '2026-05-03 02:48:57.375293');
INSERT INTO public.goose_db_version VALUES (39, 38, true, '2026-05-03 02:48:57.388484');
INSERT INTO public.goose_db_version VALUES (40, 39, true, '2026-05-03 02:48:57.391753');
INSERT INTO public.goose_db_version VALUES (41, 40, true, '2026-05-03 02:48:57.396071');
INSERT INTO public.goose_db_version VALUES (42, 41, true, '2026-05-03 02:48:57.423244');
INSERT INTO public.goose_db_version VALUES (43, 42, true, '2026-05-03 22:15:10.307328');
INSERT INTO public.goose_db_version VALUES (44, 43, true, '2026-05-06 04:47:56.807461');
INSERT INTO public.goose_db_version VALUES (45, 45, true, '2026-05-17 03:49:51.308251');
INSERT INTO public.goose_db_version VALUES (46, 46, true, '2026-05-17 03:49:51.320019');
INSERT INTO public.goose_db_version VALUES (47, 47, true, '2026-05-17 03:49:51.339871');
INSERT INTO public.goose_db_version VALUES (48, 48, true, '2026-05-17 03:49:51.342368');
INSERT INTO public.goose_db_version VALUES (49, 49, true, '2026-05-17 03:49:51.350201');
INSERT INTO public.goose_db_version VALUES (50, 50, true, '2026-05-17 03:49:51.368098');
INSERT INTO public.goose_db_version VALUES (51, 51, true, '2026-05-29 19:21:45.032946');
INSERT INTO public.goose_db_version VALUES (52, 53, true, '2026-05-29 19:21:45.056501');
INSERT INTO public.goose_db_version VALUES (53, 54, true, '2026-05-29 19:21:45.081858');
INSERT INTO public.goose_db_version VALUES (54, 55, true, '2026-05-29 19:21:45.117816');
INSERT INTO public.goose_db_version VALUES (55, 56, true, '2026-05-29 19:21:45.121218');
INSERT INTO public.goose_db_version VALUES (56, 57, true, '2026-05-29 19:21:45.136137');
INSERT INTO public.goose_db_version VALUES (57, 58, true, '2026-05-29 19:21:45.145885');
INSERT INTO public.goose_db_version VALUES (58, 59, true, '2026-05-29 19:21:45.196425');
INSERT INTO public.goose_db_version VALUES (59, 60, true, '2026-05-29 19:21:45.20315');
INSERT INTO public.goose_db_version VALUES (60, 61, true, '2026-05-29 19:21:45.206615');
INSERT INTO public.goose_db_version VALUES (61, 62, true, '2026-05-29 19:21:45.211277');
INSERT INTO public.goose_db_version VALUES (62, 63, true, '2026-05-29 19:21:45.241991');
INSERT INTO public.goose_db_version VALUES (63, 64, true, '2026-05-29 19:21:45.244325');
INSERT INTO public.goose_db_version VALUES (64, 65, true, '2026-06-18 03:31:33.127421');
INSERT INTO public.goose_db_version VALUES (65, 66, true, '2026-06-21 21:22:04.543401');
INSERT INTO public.goose_db_version VALUES (66, 67, true, '2026-06-21 21:22:04.551891');
INSERT INTO public.goose_db_version VALUES (67, 68, true, '2026-07-30 02:29:40.134255');
INSERT INTO public.goose_db_version VALUES (68, 69, true, '2026-07-30 02:29:40.184405');
INSERT INTO public.goose_db_version VALUES (69, 70, true, '2026-07-30 21:45:01.970755');
INSERT INTO public.goose_db_version VALUES (70, 71, true, '2026-07-30 21:45:01.974013');
INSERT INTO public.goose_db_version VALUES (71, 72, true, '2026-07-30 21:45:01.982593');
INSERT INTO public.goose_db_version VALUES (72, 73, true, '2026-09-02 21:54:40.900486');
INSERT INTO public.goose_db_version VALUES (73, 74, true, '2026-09-19 02:18:28.296681');
INSERT INTO public.goose_db_version VALUES (74, 75, true, '2026-09-19 02:18:28.317272');
INSERT INTO public.goose_db_version VALUES (75, 76, true, '2026-09-23 16:02:05.038678');
INSERT INTO public.goose_db_version VALUES (76, 77, true, '2026-09-23 16:02:05.039149');
INSERT INTO public.goose_db_version VALUES (77, 78, true, '2026-10-03 20:21:23.03192');
INSERT INTO public.goose_db_version VALUES (78, 79, true, '2026-10-05 01:04:59.866881');
INSERT INTO public.goose_db_version VALUES (79, 80, true, '2026-10-06 21:54:55.931878');
INSERT INTO public.goose_db_version VALUES (80, 81, true, '2026-10-08 22:12:06.185391');
INSERT INTO public.goose_db_version VALUES (81, 82, true, '2026-10-08 22:12:06.201991');
INSERT INTO public.goose_db_version VALUES (82, 83, true, '2026-10-15 17:52:30.1272');
INSERT INTO public.goose_db_version VALUES (83, 84, true, '2026-10-15 17:52:30.129667');
INSERT INTO public.goose_db_version VALUES (84, 85, true, '2026-10-15 17:52:30.13407');
INSERT INTO public.goose_db_version VALUES (85, 86, true, '2026-10-15 17:52:30.156876');
INSERT INTO public.goose_db_version VALUES (86, 87, true, '2026-10-15 17:52:30.160416');
INSERT INTO public.goose_db_version VALUES (87, 88, true, '2026-10-15 17:52:30.164138');
INSERT INTO public.goose_db_version VALUES (88, 89, true, '2026-10-15 17:52:30.166165');
INSERT INTO public.goose_db_version VALUES (89, 90, true, '2026-10-15 17:52:30.183388');
INSERT INTO public.goose_db_version VALUES (90, 91, true, '2026-10-15 17:52:30.186472');
INSERT INTO public.goose_db_version VALUES (91, 92, true, '2026-10-15 17:52:30.188701');
INSERT INTO public.goose_db_version VALUES (92, 93, true, '2026-10-15 17:52:30.189959');
INSERT INTO public.goose_db_version VALUES (93, 94, true, '2026-10-15 17:52:30.192752');
INSERT INTO public.goose_db_version VALUES (94, 95, true, '2026-10-15 17:52:30.193813');


--
-- Name: goose_db_version_id_seq; Type: SEQUENCE SET; Schema: public; Owner: -
--

SELECT pg_catalog.setval('public.goose_db_version_id_seq', 94, true);


--
-- PostgreSQL database dump complete
--

\unrestrict RD3iG3nioPArVAq5JwT8jrsJCe6eycq6eKQc2rj6Cy0OyDzEnthPLpybRHbr4BK


-- +weft partman/cron reconstruction (generated by scripts/collapse-migrations.sh; do not hand-edit)
DO $wpm$ BEGIN IF NOT EXISTS (SELECT 1 FROM partman.part_config WHERE parent_table='public.cron_results') THEN PERFORM partman.create_parent(p_parent_table:='public.cron_results',p_control:='ran_at',p_type:='range',p_interval:='1 day',p_template_table:='partman.template_public_cron_results',p_premake:=3,p_automatic_maintenance:='on',p_default_table:=false); END IF; END $wpm$;
DO $wpm$ BEGIN IF NOT EXISTS (SELECT 1 FROM partman.part_config WHERE parent_table='public.cron_results_4h') THEN PERFORM partman.create_parent(p_parent_table:='public.cron_results_4h',p_control:='bucket_start',p_type:='range',p_interval:='7 days',p_template_table:='partman.template_public_cron_results_4h',p_premake:=3,p_automatic_maintenance:='on',p_default_table:=false); END IF; END $wpm$;
DO $wpm$ BEGIN IF NOT EXISTS (SELECT 1 FROM partman.part_config WHERE parent_table='public.cron_results_daily') THEN PERFORM partman.create_parent(p_parent_table:='public.cron_results_daily',p_control:='bucket_start',p_type:='range',p_interval:='1 mon',p_template_table:='partman.template_public_cron_results_daily',p_premake:=3,p_automatic_maintenance:='on',p_default_table:=false); END IF; END $wpm$;
DO $wpm$ BEGIN IF NOT EXISTS (SELECT 1 FROM partman.part_config WHERE parent_table='public.cron_results_hourly') THEN PERFORM partman.create_parent(p_parent_table:='public.cron_results_hourly',p_control:='bucket_start',p_type:='range',p_interval:='1 day',p_template_table:='partman.template_public_cron_results_hourly',p_premake:=3,p_automatic_maintenance:='on',p_default_table:=false); END IF; END $wpm$;
DO $wpm$ BEGIN IF NOT EXISTS (SELECT 1 FROM partman.part_config WHERE parent_table='public.tmp_file_lines') THEN PERFORM partman.create_parent(p_parent_table:='public.tmp_file_lines',p_control:='updated_at',p_type:='range',p_interval:='01:00:00',p_template_table:='partman.template_public_tmp_file_lines',p_premake:=4,p_automatic_maintenance:='on',p_default_table:=false); END IF; END $wpm$;
DO $wpm$ BEGIN IF NOT EXISTS (SELECT 1 FROM partman.part_config WHERE parent_table='public.tmp_files') THEN PERFORM partman.create_parent(p_parent_table:='public.tmp_files',p_control:='updated_at',p_type:='range',p_interval:='01:00:00',p_template_table:='partman.template_public_tmp_files',p_premake:=4,p_automatic_maintenance:='on',p_default_table:=false); END IF; END $wpm$;

UPDATE partman.part_config SET retention='7 days',retention_keep_table='f',retention_keep_index='t',infinite_time_partitions='f',premake=3,automatic_maintenance='on' WHERE parent_table='public.cron_results';
UPDATE partman.part_config SET retention='35 days',retention_keep_table='f',retention_keep_index='t',infinite_time_partitions='f',premake=3,automatic_maintenance='on' WHERE parent_table='public.cron_results_4h';
UPDATE partman.part_config SET retention='400 days',retention_keep_table='f',retention_keep_index='t',infinite_time_partitions='f',premake=3,automatic_maintenance='on' WHERE parent_table='public.cron_results_daily';
UPDATE partman.part_config SET retention='14 days',retention_keep_table='f',retention_keep_index='t',infinite_time_partitions='f',premake=3,automatic_maintenance='on' WHERE parent_table='public.cron_results_hourly';
UPDATE partman.part_config SET retention='48 hours',retention_keep_table='f',retention_keep_index='t',infinite_time_partitions='f',premake=4,automatic_maintenance='on' WHERE parent_table='public.tmp_file_lines';
UPDATE partman.part_config SET retention='48 hours',retention_keep_table='f',retention_keep_index='t',infinite_time_partitions='f',premake=4,automatic_maintenance='on' WHERE parent_table='public.tmp_files';

DO $wcron$ BEGIN
  NULL;
  PERFORM cron.schedule('cron-rollup-4h','15 * * * *','CALL public.rollup_cron_4h()');
  PERFORM cron.schedule('cron-rollup-daily','30 * * * *','CALL public.rollup_cron_daily()');
  PERFORM cron.schedule('cron-rollup-hourly','*/5 * * * *','CALL public.rollup_cron_hourly()');
  PERFORM cron.schedule('partman-maintenance','*/30 * * * *','CALL partman.run_maintenance_proc()');
  PERFORM cron.schedule('usage-rollup-daily','0 * * * *','CALL public.rollup_usage_daily()');
EXCEPTION WHEN OTHERS THEN
  RAISE NOTICE 'weft schema bootstrap: cron.schedule unavailable (%), skipping cron jobs', SQLERRM;
END $wcron$;
-- +weft end partman/cron reconstruction
