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

\restrict T866diS4GuVmoS1KA0jiwxObCb4g0xFIgTlGuWmA4fg3vbHfqYZAUpmPmV6aixW

-- Dumped from database version 16.13 (Debian 16.13-1.pgdg13+1)
-- Dumped by pg_dump version 16.13 (Debian 16.13-1.pgdg13+1)

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
    'weftd'
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
BEGIN
    IF OLD.content_hash = NEW.content_hash THEN
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
        watch_id, customer_id, name, schedule, pipeline, content_hash,
        message, captured_by_user_id, authored_by_user_id, predecessor_id
    ) VALUES (
        OLD.id, OLD.customer_id, OLD.name, OLD.schedule, OLD.pipeline,
        OLD.content_hash, OLD.message, captured_by,
        OLD.created_by_user_id, last_history_id
    );

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
-- Name: capture_shell_function_history(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.capture_shell_function_history() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    last_history_id BIGINT;
    captured_by     TEXT;
    history_count   INT;
BEGIN
    -- No-op save (open/close in :w without editing): the row content
    -- is unchanged, so don't pollute history with a duplicate hash.
    IF OLD.content_hash = NEW.content_hash THEN
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
        function_id, customer_id, name, source, language, content_hash,
        message, captured_by_user_id, authored_by_user_id, predecessor_id
    ) VALUES (
        OLD.id, OLD.customer_id, OLD.name, OLD.source, OLD.language,
        OLD.content_hash, OLD.message, captured_by,
        OLD.created_by_user_id, last_history_id
    );

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
    INSERT INTO public.usage_daily (customer_name, day, bytes_ingested)
    SELECT customer_name,
           date_trunc('day', hour)::date AS day,
           SUM(bytes_ingested)           AS bytes_ingested
    FROM public.usage_hourly
    WHERE hour >= date_trunc('day', now()) - INTERVAL '3 days'
    GROUP BY customer_name, date_trunc('day', hour)::date
    ON CONFLICT (customer_name, day) DO UPDATE SET
        bytes_ingested = EXCLUDED.bytes_ingested;
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
-- Name: template_public_shard_heartbeats; Type: TABLE; Schema: partman; Owner: -
--

CREATE TABLE partman.template_public_shard_heartbeats (
    shard_id text NOT NULL,
    addr text NOT NULL,
    instance_id text,
    state text NOT NULL,
    row_count bigint NOT NULL,
    disk_bytes bigint NOT NULL,
    beat_at timestamp with time zone NOT NULL
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
    customer_id bigint NOT NULL,
    path text NOT NULL,
    is_dir boolean NOT NULL,
    byte_size bigint NOT NULL,
    line_count bigint NOT NULL,
    created_at timestamp with time zone NOT NULL,
    updated_at timestamp with time zone NOT NULL
);


--
-- Name: template_public_usage_daily; Type: TABLE; Schema: partman; Owner: -
--

CREATE TABLE partman.template_public_usage_daily (
    customer_name text NOT NULL,
    day date NOT NULL,
    bytes_ingested bigint NOT NULL
);


--
-- Name: template_public_usage_hourly; Type: TABLE; Schema: partman; Owner: -
--

CREATE TABLE partman.template_public_usage_hourly (
    customer_name text NOT NULL,
    hour timestamp with time zone NOT NULL,
    bytes_ingested bigint NOT NULL
);


--
-- Name: aws_accounts; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.aws_accounts (
    id bigint NOT NULL,
    created_at timestamp with time zone,
    updated_at timestamp with time zone,
    deleted_at timestamp with time zone,
    customer_id bigint NOT NULL,
    account_id text NOT NULL,
    role_arn text NOT NULL,
    external_id text
);


--
-- Name: accounts_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.accounts_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: accounts_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.accounts_id_seq OWNED BY public.aws_accounts.id;


--
-- Name: agent_tokens; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.agent_tokens (
    id bigint NOT NULL,
    created_at timestamp with time zone,
    updated_at timestamp with time zone,
    deleted_at timestamp with time zone,
    customer_id bigint NOT NULL,
    namespace_id bigint NOT NULL,
    token_hash text NOT NULL,
    label text,
    revoked boolean DEFAULT false NOT NULL,
    metrics_enabled boolean DEFAULT false NOT NULL,
    metrics_interval text,
    metrics_collectors text,
    prefix text NOT NULL,
    env text DEFAULT ''::text NOT NULL
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
    apps text NOT NULL,
    target_ingesting_count integer NOT NULL,
    tier text DEFAULT 'small'::text NOT NULL,
    created_at text NOT NULL,
    window_duration text DEFAULT '168h'::text NOT NULL,
    retention_period text DEFAULT '168h'::text NOT NULL,
    label text NOT NULL,
    id bigint NOT NULL
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
-- Name: cron_results_4h_p20260412; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.cron_results_4h_p20260412 (
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
-- Name: cron_results_4h_p20260419; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.cron_results_4h_p20260419 (
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
-- Name: cron_results_4h_p20260426; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.cron_results_4h_p20260426 (
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
-- Name: cron_results_4h_p20260503; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.cron_results_4h_p20260503 (
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
-- Name: cron_results_4h_p20260510; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.cron_results_4h_p20260510 (
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
-- Name: cron_results_4h_p20260517; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.cron_results_4h_p20260517 (
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
-- Name: cron_results_4h_p20260524; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.cron_results_4h_p20260524 (
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
-- Name: cron_results_daily_p20260201; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.cron_results_daily_p20260201 (
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
-- Name: cron_results_daily_p20260301; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.cron_results_daily_p20260301 (
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
-- Name: cron_results_daily_p20260401; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.cron_results_daily_p20260401 (
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
-- Name: cron_results_daily_p20260501; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.cron_results_daily_p20260501 (
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
-- Name: cron_results_daily_p20260601; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.cron_results_daily_p20260601 (
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
-- Name: cron_results_daily_p20260701; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.cron_results_daily_p20260701 (
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
-- Name: cron_results_daily_p20260801; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.cron_results_daily_p20260801 (
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
-- Name: cron_results_hourly_p20260430; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.cron_results_hourly_p20260430 (
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
-- Name: cron_results_hourly_p20260501; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.cron_results_hourly_p20260501 (
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
-- Name: cron_results_hourly_p20260502; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.cron_results_hourly_p20260502 (
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
-- Name: cron_results_hourly_p20260503; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.cron_results_hourly_p20260503 (
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
-- Name: cron_results_hourly_p20260504; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.cron_results_hourly_p20260504 (
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
-- Name: cron_results_hourly_p20260505; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.cron_results_hourly_p20260505 (
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
-- Name: cron_results_hourly_p20260506; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.cron_results_hourly_p20260506 (
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
-- Name: cron_results_p20260430; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.cron_results_p20260430 (
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
-- Name: cron_results_p20260501; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.cron_results_p20260501 (
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
-- Name: cron_results_p20260502; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.cron_results_p20260502 (
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
-- Name: cron_results_p20260503; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.cron_results_p20260503 (
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
-- Name: cron_results_p20260504; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.cron_results_p20260504 (
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
-- Name: cron_results_p20260505; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.cron_results_p20260505 (
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
-- Name: cron_results_p20260506; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.cron_results_p20260506 (
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
-- Name: cron_watch_history; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.cron_watch_history (
    id bigint NOT NULL,
    watch_id bigint NOT NULL,
    customer_id bigint NOT NULL,
    name text NOT NULL,
    schedule text NOT NULL,
    pipeline text NOT NULL,
    content_hash text NOT NULL,
    message text DEFAULT ''::text NOT NULL,
    captured_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    captured_by_user_id text,
    authored_by_user_id text,
    predecessor_id bigint
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
-- Name: cron_watches; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.cron_watches (
    id bigint NOT NULL,
    created_at timestamp with time zone,
    updated_at timestamp with time zone,
    deleted_at timestamp with time zone,
    customer_id bigint NOT NULL,
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
    message text DEFAULT ''::text NOT NULL
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
-- Name: customers; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.customers (
    id bigint NOT NULL,
    created_at timestamp with time zone,
    updated_at timestamp with time zone,
    deleted_at timestamp with time zone,
    name text NOT NULL,
    display_name text
);


--
-- Name: customers_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.customers_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: customers_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.customers_id_seq OWNED BY public.customers.id;


--
-- Name: dashboards; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.dashboards (
    id bigint NOT NULL,
    created_at timestamp with time zone,
    updated_at timestamp with time zone,
    deleted_at timestamp with time zone,
    customer_id bigint NOT NULL,
    name text NOT NULL,
    yaml_body text NOT NULL,
    created_by_user_id text
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
-- Name: log_groups; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.log_groups (
    id bigint NOT NULL,
    created_at timestamp with time zone,
    updated_at timestamp with time zone,
    deleted_at timestamp with time zone,
    aws_account_id bigint,
    app text NOT NULL,
    log_group_name text,
    region text DEFAULT 'us-east-2'::text NOT NULL,
    schema_yaml text,
    poll_interval text DEFAULT '30s'::text NOT NULL,
    enabled boolean DEFAULT true NOT NULL,
    last_poll_at text,
    last_token text,
    source_type public.log_group_source_type NOT NULL,
    namespace_id bigint NOT NULL,
    status text DEFAULT 'configured'::text NOT NULL,
    instance text,
    deployment text,
    sample text,
    detection text,
    CONSTRAINT log_groups_status_check CHECK ((status = ANY (ARRAY['pending'::text, 'configured'::text])))
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
    can_create_tokens boolean DEFAULT false NOT NULL,
    can_manage_watches boolean DEFAULT false NOT NULL,
    can_manage_functions boolean DEFAULT false NOT NULL,
    can_manage_integrations boolean DEFAULT false NOT NULL
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
    customer_id bigint NOT NULL,
    name text NOT NULL
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
-- Name: notify_configs; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.notify_configs (
    id bigint NOT NULL,
    created_at timestamp with time zone,
    updated_at timestamp with time zone,
    deleted_at timestamp with time zone,
    customer_id bigint NOT NULL,
    slack_webhook text,
    slack_bot_token text,
    slack_team_name text,
    configured_at timestamp with time zone,
    aliases_yaml text DEFAULT ''::text NOT NULL
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
-- Name: pagerduty_integrations; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.pagerduty_integrations (
    id bigint NOT NULL,
    created_at timestamp with time zone DEFAULT now(),
    updated_at timestamp with time zone DEFAULT now(),
    deleted_at timestamp with time zone,
    customer_id bigint NOT NULL,
    name text NOT NULL,
    integration_key text NOT NULL,
    last_used_at timestamp with time zone,
    last_error text
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
-- Name: saved_queries; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.saved_queries (
    id text NOT NULL,
    user_id text NOT NULL,
    name text NOT NULL,
    query text NOT NULL,
    created_at timestamp with time zone,
    updated_at timestamp with time zone
);


--
-- Name: shard_heartbeats; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.shard_heartbeats (
    shard_id text NOT NULL,
    addr text NOT NULL,
    instance_id text,
    state text NOT NULL,
    row_count bigint DEFAULT 0 NOT NULL,
    disk_bytes bigint DEFAULT 0 NOT NULL,
    beat_at timestamp with time zone NOT NULL
)
PARTITION BY RANGE (beat_at);


--
-- Name: shard_heartbeats_default; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.shard_heartbeats_default (
    shard_id text NOT NULL,
    addr text NOT NULL,
    instance_id text,
    state text NOT NULL,
    row_count bigint DEFAULT 0 NOT NULL,
    disk_bytes bigint DEFAULT 0 NOT NULL,
    beat_at timestamp with time zone NOT NULL
);


--
-- Name: shard_heartbeats_p20260503_000000; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.shard_heartbeats_p20260503_000000 (
    shard_id text NOT NULL,
    addr text NOT NULL,
    instance_id text,
    state text NOT NULL,
    row_count bigint DEFAULT 0 NOT NULL,
    disk_bytes bigint DEFAULT 0 NOT NULL,
    beat_at timestamp with time zone NOT NULL
);


--
-- Name: shard_heartbeats_p20260503_010000; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.shard_heartbeats_p20260503_010000 (
    shard_id text NOT NULL,
    addr text NOT NULL,
    instance_id text,
    state text NOT NULL,
    row_count bigint DEFAULT 0 NOT NULL,
    disk_bytes bigint DEFAULT 0 NOT NULL,
    beat_at timestamp with time zone NOT NULL
);


--
-- Name: shard_heartbeats_p20260503_020000; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.shard_heartbeats_p20260503_020000 (
    shard_id text NOT NULL,
    addr text NOT NULL,
    instance_id text,
    state text NOT NULL,
    row_count bigint DEFAULT 0 NOT NULL,
    disk_bytes bigint DEFAULT 0 NOT NULL,
    beat_at timestamp with time zone NOT NULL
);


--
-- Name: shard_heartbeats_p20260503_030000; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.shard_heartbeats_p20260503_030000 (
    shard_id text NOT NULL,
    addr text NOT NULL,
    instance_id text,
    state text NOT NULL,
    row_count bigint DEFAULT 0 NOT NULL,
    disk_bytes bigint DEFAULT 0 NOT NULL,
    beat_at timestamp with time zone NOT NULL
);


--
-- Name: shard_heartbeats_p20260503_040000; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.shard_heartbeats_p20260503_040000 (
    shard_id text NOT NULL,
    addr text NOT NULL,
    instance_id text,
    state text NOT NULL,
    row_count bigint DEFAULT 0 NOT NULL,
    disk_bytes bigint DEFAULT 0 NOT NULL,
    beat_at timestamp with time zone NOT NULL
);


--
-- Name: shard_records; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.shard_records (
    id text NOT NULL,
    apps text NOT NULL,
    window_start text NOT NULL,
    window_end text NOT NULL,
    state text NOT NULL,
    addr text,
    instance_id text,
    parquet_path text,
    tier text,
    row_count bigint DEFAULT 0,
    created_at text NOT NULL,
    ebs_volume_id text,
    heartbeat_token text,
    bucket_id bigint NOT NULL
);


--
-- Name: shell_function_history; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.shell_function_history (
    id bigint NOT NULL,
    function_id bigint NOT NULL,
    customer_id bigint NOT NULL,
    name text NOT NULL,
    source text NOT NULL,
    language text NOT NULL,
    content_hash text NOT NULL,
    message text DEFAULT ''::text NOT NULL,
    captured_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    captured_by_user_id text,
    authored_by_user_id text,
    predecessor_id bigint
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
    customer_id bigint NOT NULL,
    name text NOT NULL,
    source text NOT NULL,
    language text DEFAULT 'bash'::text NOT NULL,
    created_by_user_id text,
    content_hash text DEFAULT ''::text NOT NULL,
    message text DEFAULT ''::text NOT NULL
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
    customer_name text NOT NULL,
    day date NOT NULL,
    kb_reported bigint NOT NULL,
    stripe_event_id text NOT NULL,
    reported_at timestamp with time zone DEFAULT now() NOT NULL
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
    customer_id text NOT NULL,
    tier text NOT NULL,
    status text NOT NULL,
    starts_at timestamp with time zone NOT NULL,
    expires_at timestamp with time zone,
    stripe_subscription_id text,
    created_at timestamp with time zone,
    updated_at timestamp with time zone,
    comp_reason text DEFAULT ''::text NOT NULL
);


--
-- Name: team_members; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.team_members (
    id text NOT NULL,
    email text NOT NULL,
    user_id text,
    role text DEFAULT 'member'::text NOT NULL,
    status text DEFAULT 'pending'::text NOT NULL,
    invited_by text,
    invited_at timestamp with time zone,
    accepted_at timestamp with time zone,
    created_at timestamp with time zone,
    updated_at timestamp with time zone,
    customer_id bigint NOT NULL
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
-- Name: tmp_file_lines_p20260502_220000; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.tmp_file_lines_p20260502_220000 (
    file_id bigint NOT NULL,
    line_num bigint NOT NULL,
    content text NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: tmp_file_lines_p20260502_230000; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.tmp_file_lines_p20260502_230000 (
    file_id bigint NOT NULL,
    line_num bigint NOT NULL,
    content text NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: tmp_file_lines_p20260503_000000; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.tmp_file_lines_p20260503_000000 (
    file_id bigint NOT NULL,
    line_num bigint NOT NULL,
    content text NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: tmp_file_lines_p20260503_010000; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.tmp_file_lines_p20260503_010000 (
    file_id bigint NOT NULL,
    line_num bigint NOT NULL,
    content text NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: tmp_file_lines_p20260503_020000; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.tmp_file_lines_p20260503_020000 (
    file_id bigint NOT NULL,
    line_num bigint NOT NULL,
    content text NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: tmp_file_lines_p20260503_030000; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.tmp_file_lines_p20260503_030000 (
    file_id bigint NOT NULL,
    line_num bigint NOT NULL,
    content text NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: tmp_file_lines_p20260503_040000; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.tmp_file_lines_p20260503_040000 (
    file_id bigint NOT NULL,
    line_num bigint NOT NULL,
    content text NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: tmp_file_lines_p20260503_050000; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.tmp_file_lines_p20260503_050000 (
    file_id bigint NOT NULL,
    line_num bigint NOT NULL,
    content text NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: tmp_file_lines_p20260503_060000; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.tmp_file_lines_p20260503_060000 (
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
    customer_id bigint NOT NULL,
    path text NOT NULL,
    is_dir boolean DEFAULT false NOT NULL,
    byte_size bigint DEFAULT 0 NOT NULL,
    line_count bigint DEFAULT 0 NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    created_by_user_id text
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
    customer_id bigint NOT NULL,
    path text NOT NULL,
    is_dir boolean DEFAULT false NOT NULL,
    byte_size bigint DEFAULT 0 NOT NULL,
    line_count bigint DEFAULT 0 NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    created_by_user_id text
);


--
-- Name: tmp_files_p20260502_220000; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.tmp_files_p20260502_220000 (
    id bigint DEFAULT nextval('public.tmp_files_id_seq'::regclass) NOT NULL,
    customer_id bigint NOT NULL,
    path text NOT NULL,
    is_dir boolean DEFAULT false NOT NULL,
    byte_size bigint DEFAULT 0 NOT NULL,
    line_count bigint DEFAULT 0 NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    created_by_user_id text
);


--
-- Name: tmp_files_p20260502_230000; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.tmp_files_p20260502_230000 (
    id bigint DEFAULT nextval('public.tmp_files_id_seq'::regclass) NOT NULL,
    customer_id bigint NOT NULL,
    path text NOT NULL,
    is_dir boolean DEFAULT false NOT NULL,
    byte_size bigint DEFAULT 0 NOT NULL,
    line_count bigint DEFAULT 0 NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    created_by_user_id text
);


--
-- Name: tmp_files_p20260503_000000; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.tmp_files_p20260503_000000 (
    id bigint DEFAULT nextval('public.tmp_files_id_seq'::regclass) NOT NULL,
    customer_id bigint NOT NULL,
    path text NOT NULL,
    is_dir boolean DEFAULT false NOT NULL,
    byte_size bigint DEFAULT 0 NOT NULL,
    line_count bigint DEFAULT 0 NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    created_by_user_id text
);


--
-- Name: tmp_files_p20260503_010000; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.tmp_files_p20260503_010000 (
    id bigint DEFAULT nextval('public.tmp_files_id_seq'::regclass) NOT NULL,
    customer_id bigint NOT NULL,
    path text NOT NULL,
    is_dir boolean DEFAULT false NOT NULL,
    byte_size bigint DEFAULT 0 NOT NULL,
    line_count bigint DEFAULT 0 NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    created_by_user_id text
);


--
-- Name: tmp_files_p20260503_020000; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.tmp_files_p20260503_020000 (
    id bigint DEFAULT nextval('public.tmp_files_id_seq'::regclass) NOT NULL,
    customer_id bigint NOT NULL,
    path text NOT NULL,
    is_dir boolean DEFAULT false NOT NULL,
    byte_size bigint DEFAULT 0 NOT NULL,
    line_count bigint DEFAULT 0 NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    created_by_user_id text
);


--
-- Name: tmp_files_p20260503_030000; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.tmp_files_p20260503_030000 (
    id bigint DEFAULT nextval('public.tmp_files_id_seq'::regclass) NOT NULL,
    customer_id bigint NOT NULL,
    path text NOT NULL,
    is_dir boolean DEFAULT false NOT NULL,
    byte_size bigint DEFAULT 0 NOT NULL,
    line_count bigint DEFAULT 0 NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    created_by_user_id text
);


--
-- Name: tmp_files_p20260503_040000; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.tmp_files_p20260503_040000 (
    id bigint DEFAULT nextval('public.tmp_files_id_seq'::regclass) NOT NULL,
    customer_id bigint NOT NULL,
    path text NOT NULL,
    is_dir boolean DEFAULT false NOT NULL,
    byte_size bigint DEFAULT 0 NOT NULL,
    line_count bigint DEFAULT 0 NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    created_by_user_id text
);


--
-- Name: tmp_files_p20260503_050000; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.tmp_files_p20260503_050000 (
    id bigint DEFAULT nextval('public.tmp_files_id_seq'::regclass) NOT NULL,
    customer_id bigint NOT NULL,
    path text NOT NULL,
    is_dir boolean DEFAULT false NOT NULL,
    byte_size bigint DEFAULT 0 NOT NULL,
    line_count bigint DEFAULT 0 NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    created_by_user_id text
);


--
-- Name: tmp_files_p20260503_060000; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.tmp_files_p20260503_060000 (
    id bigint DEFAULT nextval('public.tmp_files_id_seq'::regclass) NOT NULL,
    customer_id bigint NOT NULL,
    path text NOT NULL,
    is_dir boolean DEFAULT false NOT NULL,
    byte_size bigint DEFAULT 0 NOT NULL,
    line_count bigint DEFAULT 0 NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    created_by_user_id text
);


--
-- Name: usage_daily; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.usage_daily (
    customer_name text NOT NULL,
    day date NOT NULL,
    bytes_ingested bigint DEFAULT 0 NOT NULL
)
PARTITION BY RANGE (day);


--
-- Name: usage_daily_default; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.usage_daily_default (
    customer_name text NOT NULL,
    day date NOT NULL,
    bytes_ingested bigint DEFAULT 0 NOT NULL
);


--
-- Name: usage_daily_p20260201; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.usage_daily_p20260201 (
    customer_name text NOT NULL,
    day date NOT NULL,
    bytes_ingested bigint DEFAULT 0 NOT NULL
);


--
-- Name: usage_daily_p20260301; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.usage_daily_p20260301 (
    customer_name text NOT NULL,
    day date NOT NULL,
    bytes_ingested bigint DEFAULT 0 NOT NULL
);


--
-- Name: usage_daily_p20260401; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.usage_daily_p20260401 (
    customer_name text NOT NULL,
    day date NOT NULL,
    bytes_ingested bigint DEFAULT 0 NOT NULL
);


--
-- Name: usage_daily_p20260501; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.usage_daily_p20260501 (
    customer_name text NOT NULL,
    day date NOT NULL,
    bytes_ingested bigint DEFAULT 0 NOT NULL
);


--
-- Name: usage_daily_p20260601; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.usage_daily_p20260601 (
    customer_name text NOT NULL,
    day date NOT NULL,
    bytes_ingested bigint DEFAULT 0 NOT NULL
);


--
-- Name: usage_daily_p20260701; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.usage_daily_p20260701 (
    customer_name text NOT NULL,
    day date NOT NULL,
    bytes_ingested bigint DEFAULT 0 NOT NULL
);


--
-- Name: usage_daily_p20260801; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.usage_daily_p20260801 (
    customer_name text NOT NULL,
    day date NOT NULL,
    bytes_ingested bigint DEFAULT 0 NOT NULL
);


--
-- Name: usage_hourly; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.usage_hourly (
    customer_name text NOT NULL,
    hour timestamp with time zone NOT NULL,
    bytes_ingested bigint DEFAULT 0 NOT NULL
)
PARTITION BY RANGE (hour);


--
-- Name: usage_hourly_default; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.usage_hourly_default (
    customer_name text NOT NULL,
    hour timestamp with time zone NOT NULL,
    bytes_ingested bigint DEFAULT 0 NOT NULL
);


--
-- Name: usage_hourly_p20260430; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.usage_hourly_p20260430 (
    customer_name text NOT NULL,
    hour timestamp with time zone NOT NULL,
    bytes_ingested bigint DEFAULT 0 NOT NULL
);


--
-- Name: usage_hourly_p20260501; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.usage_hourly_p20260501 (
    customer_name text NOT NULL,
    hour timestamp with time zone NOT NULL,
    bytes_ingested bigint DEFAULT 0 NOT NULL
);


--
-- Name: usage_hourly_p20260502; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.usage_hourly_p20260502 (
    customer_name text NOT NULL,
    hour timestamp with time zone NOT NULL,
    bytes_ingested bigint DEFAULT 0 NOT NULL
);


--
-- Name: usage_hourly_p20260503; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.usage_hourly_p20260503 (
    customer_name text NOT NULL,
    hour timestamp with time zone NOT NULL,
    bytes_ingested bigint DEFAULT 0 NOT NULL
);


--
-- Name: usage_hourly_p20260504; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.usage_hourly_p20260504 (
    customer_name text NOT NULL,
    hour timestamp with time zone NOT NULL,
    bytes_ingested bigint DEFAULT 0 NOT NULL
);


--
-- Name: usage_hourly_p20260505; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.usage_hourly_p20260505 (
    customer_name text NOT NULL,
    hour timestamp with time zone NOT NULL,
    bytes_ingested bigint DEFAULT 0 NOT NULL
);


--
-- Name: usage_hourly_p20260506; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.usage_hourly_p20260506 (
    customer_name text NOT NULL,
    hour timestamp with time zone NOT NULL,
    bytes_ingested bigint DEFAULT 0 NOT NULL
);


--
-- Name: user_notices; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.user_notices (
    user_id text NOT NULL,
    notice_key text NOT NULL,
    kind public.notice_kind NOT NULL,
    seen_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: users; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.users (
    id text NOT NULL,
    email text NOT NULL,
    name text,
    stripe_customer_id text,
    can_create_namespace boolean DEFAULT false NOT NULL,
    created_at timestamp with time zone,
    updated_at timestamp with time zone,
    email_verified_at timestamp with time zone,
    has_seen_welcome boolean DEFAULT false NOT NULL,
    last_active_customer_id bigint
);


--
-- Name: cron_results_4h_default; Type: TABLE ATTACH; Schema: public; Owner: -
--

ALTER TABLE ONLY public.cron_results_4h ATTACH PARTITION public.cron_results_4h_default DEFAULT;


--
-- Name: cron_results_4h_p20260412; Type: TABLE ATTACH; Schema: public; Owner: -
--

ALTER TABLE ONLY public.cron_results_4h ATTACH PARTITION public.cron_results_4h_p20260412 FOR VALUES FROM ('2026-04-12 00:00:00+00') TO ('2026-04-19 00:00:00+00');


--
-- Name: cron_results_4h_p20260419; Type: TABLE ATTACH; Schema: public; Owner: -
--

ALTER TABLE ONLY public.cron_results_4h ATTACH PARTITION public.cron_results_4h_p20260419 FOR VALUES FROM ('2026-04-19 00:00:00+00') TO ('2026-04-26 00:00:00+00');


--
-- Name: cron_results_4h_p20260426; Type: TABLE ATTACH; Schema: public; Owner: -
--

ALTER TABLE ONLY public.cron_results_4h ATTACH PARTITION public.cron_results_4h_p20260426 FOR VALUES FROM ('2026-04-26 00:00:00+00') TO ('2026-05-03 00:00:00+00');


--
-- Name: cron_results_4h_p20260503; Type: TABLE ATTACH; Schema: public; Owner: -
--

ALTER TABLE ONLY public.cron_results_4h ATTACH PARTITION public.cron_results_4h_p20260503 FOR VALUES FROM ('2026-05-03 00:00:00+00') TO ('2026-05-10 00:00:00+00');


--
-- Name: cron_results_4h_p20260510; Type: TABLE ATTACH; Schema: public; Owner: -
--

ALTER TABLE ONLY public.cron_results_4h ATTACH PARTITION public.cron_results_4h_p20260510 FOR VALUES FROM ('2026-05-10 00:00:00+00') TO ('2026-05-17 00:00:00+00');


--
-- Name: cron_results_4h_p20260517; Type: TABLE ATTACH; Schema: public; Owner: -
--

ALTER TABLE ONLY public.cron_results_4h ATTACH PARTITION public.cron_results_4h_p20260517 FOR VALUES FROM ('2026-05-17 00:00:00+00') TO ('2026-05-24 00:00:00+00');


--
-- Name: cron_results_4h_p20260524; Type: TABLE ATTACH; Schema: public; Owner: -
--

ALTER TABLE ONLY public.cron_results_4h ATTACH PARTITION public.cron_results_4h_p20260524 FOR VALUES FROM ('2026-05-24 00:00:00+00') TO ('2026-05-31 00:00:00+00');


--
-- Name: cron_results_daily_default; Type: TABLE ATTACH; Schema: public; Owner: -
--

ALTER TABLE ONLY public.cron_results_daily ATTACH PARTITION public.cron_results_daily_default DEFAULT;


--
-- Name: cron_results_daily_p20260201; Type: TABLE ATTACH; Schema: public; Owner: -
--

ALTER TABLE ONLY public.cron_results_daily ATTACH PARTITION public.cron_results_daily_p20260201 FOR VALUES FROM ('2026-02-01 00:00:00+00') TO ('2026-03-01 00:00:00+00');


--
-- Name: cron_results_daily_p20260301; Type: TABLE ATTACH; Schema: public; Owner: -
--

ALTER TABLE ONLY public.cron_results_daily ATTACH PARTITION public.cron_results_daily_p20260301 FOR VALUES FROM ('2026-03-01 00:00:00+00') TO ('2026-04-01 00:00:00+00');


--
-- Name: cron_results_daily_p20260401; Type: TABLE ATTACH; Schema: public; Owner: -
--

ALTER TABLE ONLY public.cron_results_daily ATTACH PARTITION public.cron_results_daily_p20260401 FOR VALUES FROM ('2026-04-01 00:00:00+00') TO ('2026-05-01 00:00:00+00');


--
-- Name: cron_results_daily_p20260501; Type: TABLE ATTACH; Schema: public; Owner: -
--

ALTER TABLE ONLY public.cron_results_daily ATTACH PARTITION public.cron_results_daily_p20260501 FOR VALUES FROM ('2026-05-01 00:00:00+00') TO ('2026-06-01 00:00:00+00');


--
-- Name: cron_results_daily_p20260601; Type: TABLE ATTACH; Schema: public; Owner: -
--

ALTER TABLE ONLY public.cron_results_daily ATTACH PARTITION public.cron_results_daily_p20260601 FOR VALUES FROM ('2026-06-01 00:00:00+00') TO ('2026-07-01 00:00:00+00');


--
-- Name: cron_results_daily_p20260701; Type: TABLE ATTACH; Schema: public; Owner: -
--

ALTER TABLE ONLY public.cron_results_daily ATTACH PARTITION public.cron_results_daily_p20260701 FOR VALUES FROM ('2026-07-01 00:00:00+00') TO ('2026-08-01 00:00:00+00');


--
-- Name: cron_results_daily_p20260801; Type: TABLE ATTACH; Schema: public; Owner: -
--

ALTER TABLE ONLY public.cron_results_daily ATTACH PARTITION public.cron_results_daily_p20260801 FOR VALUES FROM ('2026-08-01 00:00:00+00') TO ('2026-09-01 00:00:00+00');


--
-- Name: cron_results_default; Type: TABLE ATTACH; Schema: public; Owner: -
--

ALTER TABLE ONLY public.cron_results ATTACH PARTITION public.cron_results_default DEFAULT;


--
-- Name: cron_results_hourly_default; Type: TABLE ATTACH; Schema: public; Owner: -
--

ALTER TABLE ONLY public.cron_results_hourly ATTACH PARTITION public.cron_results_hourly_default DEFAULT;


--
-- Name: cron_results_hourly_p20260430; Type: TABLE ATTACH; Schema: public; Owner: -
--

ALTER TABLE ONLY public.cron_results_hourly ATTACH PARTITION public.cron_results_hourly_p20260430 FOR VALUES FROM ('2026-04-30 00:00:00+00') TO ('2026-05-01 00:00:00+00');


--
-- Name: cron_results_hourly_p20260501; Type: TABLE ATTACH; Schema: public; Owner: -
--

ALTER TABLE ONLY public.cron_results_hourly ATTACH PARTITION public.cron_results_hourly_p20260501 FOR VALUES FROM ('2026-05-01 00:00:00+00') TO ('2026-05-02 00:00:00+00');


--
-- Name: cron_results_hourly_p20260502; Type: TABLE ATTACH; Schema: public; Owner: -
--

ALTER TABLE ONLY public.cron_results_hourly ATTACH PARTITION public.cron_results_hourly_p20260502 FOR VALUES FROM ('2026-05-02 00:00:00+00') TO ('2026-05-03 00:00:00+00');


--
-- Name: cron_results_hourly_p20260503; Type: TABLE ATTACH; Schema: public; Owner: -
--

ALTER TABLE ONLY public.cron_results_hourly ATTACH PARTITION public.cron_results_hourly_p20260503 FOR VALUES FROM ('2026-05-03 00:00:00+00') TO ('2026-05-04 00:00:00+00');


--
-- Name: cron_results_hourly_p20260504; Type: TABLE ATTACH; Schema: public; Owner: -
--

ALTER TABLE ONLY public.cron_results_hourly ATTACH PARTITION public.cron_results_hourly_p20260504 FOR VALUES FROM ('2026-05-04 00:00:00+00') TO ('2026-05-05 00:00:00+00');


--
-- Name: cron_results_hourly_p20260505; Type: TABLE ATTACH; Schema: public; Owner: -
--

ALTER TABLE ONLY public.cron_results_hourly ATTACH PARTITION public.cron_results_hourly_p20260505 FOR VALUES FROM ('2026-05-05 00:00:00+00') TO ('2026-05-06 00:00:00+00');


--
-- Name: cron_results_hourly_p20260506; Type: TABLE ATTACH; Schema: public; Owner: -
--

ALTER TABLE ONLY public.cron_results_hourly ATTACH PARTITION public.cron_results_hourly_p20260506 FOR VALUES FROM ('2026-05-06 00:00:00+00') TO ('2026-05-07 00:00:00+00');


--
-- Name: cron_results_p20260430; Type: TABLE ATTACH; Schema: public; Owner: -
--

ALTER TABLE ONLY public.cron_results ATTACH PARTITION public.cron_results_p20260430 FOR VALUES FROM ('2026-04-30 00:00:00+00') TO ('2026-05-01 00:00:00+00');


--
-- Name: cron_results_p20260501; Type: TABLE ATTACH; Schema: public; Owner: -
--

ALTER TABLE ONLY public.cron_results ATTACH PARTITION public.cron_results_p20260501 FOR VALUES FROM ('2026-05-01 00:00:00+00') TO ('2026-05-02 00:00:00+00');


--
-- Name: cron_results_p20260502; Type: TABLE ATTACH; Schema: public; Owner: -
--

ALTER TABLE ONLY public.cron_results ATTACH PARTITION public.cron_results_p20260502 FOR VALUES FROM ('2026-05-02 00:00:00+00') TO ('2026-05-03 00:00:00+00');


--
-- Name: cron_results_p20260503; Type: TABLE ATTACH; Schema: public; Owner: -
--

ALTER TABLE ONLY public.cron_results ATTACH PARTITION public.cron_results_p20260503 FOR VALUES FROM ('2026-05-03 00:00:00+00') TO ('2026-05-04 00:00:00+00');


--
-- Name: cron_results_p20260504; Type: TABLE ATTACH; Schema: public; Owner: -
--

ALTER TABLE ONLY public.cron_results ATTACH PARTITION public.cron_results_p20260504 FOR VALUES FROM ('2026-05-04 00:00:00+00') TO ('2026-05-05 00:00:00+00');


--
-- Name: cron_results_p20260505; Type: TABLE ATTACH; Schema: public; Owner: -
--

ALTER TABLE ONLY public.cron_results ATTACH PARTITION public.cron_results_p20260505 FOR VALUES FROM ('2026-05-05 00:00:00+00') TO ('2026-05-06 00:00:00+00');


--
-- Name: cron_results_p20260506; Type: TABLE ATTACH; Schema: public; Owner: -
--

ALTER TABLE ONLY public.cron_results ATTACH PARTITION public.cron_results_p20260506 FOR VALUES FROM ('2026-05-06 00:00:00+00') TO ('2026-05-07 00:00:00+00');


--
-- Name: shard_heartbeats_default; Type: TABLE ATTACH; Schema: public; Owner: -
--

ALTER TABLE ONLY public.shard_heartbeats ATTACH PARTITION public.shard_heartbeats_default DEFAULT;


--
-- Name: shard_heartbeats_p20260503_000000; Type: TABLE ATTACH; Schema: public; Owner: -
--

ALTER TABLE ONLY public.shard_heartbeats ATTACH PARTITION public.shard_heartbeats_p20260503_000000 FOR VALUES FROM ('2026-05-03 00:00:00+00') TO ('2026-05-03 01:00:00+00');


--
-- Name: shard_heartbeats_p20260503_010000; Type: TABLE ATTACH; Schema: public; Owner: -
--

ALTER TABLE ONLY public.shard_heartbeats ATTACH PARTITION public.shard_heartbeats_p20260503_010000 FOR VALUES FROM ('2026-05-03 01:00:00+00') TO ('2026-05-03 02:00:00+00');


--
-- Name: shard_heartbeats_p20260503_020000; Type: TABLE ATTACH; Schema: public; Owner: -
--

ALTER TABLE ONLY public.shard_heartbeats ATTACH PARTITION public.shard_heartbeats_p20260503_020000 FOR VALUES FROM ('2026-05-03 02:00:00+00') TO ('2026-05-03 03:00:00+00');


--
-- Name: shard_heartbeats_p20260503_030000; Type: TABLE ATTACH; Schema: public; Owner: -
--

ALTER TABLE ONLY public.shard_heartbeats ATTACH PARTITION public.shard_heartbeats_p20260503_030000 FOR VALUES FROM ('2026-05-03 03:00:00+00') TO ('2026-05-03 04:00:00+00');


--
-- Name: shard_heartbeats_p20260503_040000; Type: TABLE ATTACH; Schema: public; Owner: -
--

ALTER TABLE ONLY public.shard_heartbeats ATTACH PARTITION public.shard_heartbeats_p20260503_040000 FOR VALUES FROM ('2026-05-03 04:00:00+00') TO ('2026-05-03 05:00:00+00');


--
-- Name: tmp_file_lines_default; Type: TABLE ATTACH; Schema: public; Owner: -
--

ALTER TABLE ONLY public.tmp_file_lines ATTACH PARTITION public.tmp_file_lines_default DEFAULT;


--
-- Name: tmp_file_lines_p20260502_220000; Type: TABLE ATTACH; Schema: public; Owner: -
--

ALTER TABLE ONLY public.tmp_file_lines ATTACH PARTITION public.tmp_file_lines_p20260502_220000 FOR VALUES FROM ('2026-05-02 22:00:00+00') TO ('2026-05-02 23:00:00+00');


--
-- Name: tmp_file_lines_p20260502_230000; Type: TABLE ATTACH; Schema: public; Owner: -
--

ALTER TABLE ONLY public.tmp_file_lines ATTACH PARTITION public.tmp_file_lines_p20260502_230000 FOR VALUES FROM ('2026-05-02 23:00:00+00') TO ('2026-05-03 00:00:00+00');


--
-- Name: tmp_file_lines_p20260503_000000; Type: TABLE ATTACH; Schema: public; Owner: -
--

ALTER TABLE ONLY public.tmp_file_lines ATTACH PARTITION public.tmp_file_lines_p20260503_000000 FOR VALUES FROM ('2026-05-03 00:00:00+00') TO ('2026-05-03 01:00:00+00');


--
-- Name: tmp_file_lines_p20260503_010000; Type: TABLE ATTACH; Schema: public; Owner: -
--

ALTER TABLE ONLY public.tmp_file_lines ATTACH PARTITION public.tmp_file_lines_p20260503_010000 FOR VALUES FROM ('2026-05-03 01:00:00+00') TO ('2026-05-03 02:00:00+00');


--
-- Name: tmp_file_lines_p20260503_020000; Type: TABLE ATTACH; Schema: public; Owner: -
--

ALTER TABLE ONLY public.tmp_file_lines ATTACH PARTITION public.tmp_file_lines_p20260503_020000 FOR VALUES FROM ('2026-05-03 02:00:00+00') TO ('2026-05-03 03:00:00+00');


--
-- Name: tmp_file_lines_p20260503_030000; Type: TABLE ATTACH; Schema: public; Owner: -
--

ALTER TABLE ONLY public.tmp_file_lines ATTACH PARTITION public.tmp_file_lines_p20260503_030000 FOR VALUES FROM ('2026-05-03 03:00:00+00') TO ('2026-05-03 04:00:00+00');


--
-- Name: tmp_file_lines_p20260503_040000; Type: TABLE ATTACH; Schema: public; Owner: -
--

ALTER TABLE ONLY public.tmp_file_lines ATTACH PARTITION public.tmp_file_lines_p20260503_040000 FOR VALUES FROM ('2026-05-03 04:00:00+00') TO ('2026-05-03 05:00:00+00');


--
-- Name: tmp_file_lines_p20260503_050000; Type: TABLE ATTACH; Schema: public; Owner: -
--

ALTER TABLE ONLY public.tmp_file_lines ATTACH PARTITION public.tmp_file_lines_p20260503_050000 FOR VALUES FROM ('2026-05-03 05:00:00+00') TO ('2026-05-03 06:00:00+00');


--
-- Name: tmp_file_lines_p20260503_060000; Type: TABLE ATTACH; Schema: public; Owner: -
--

ALTER TABLE ONLY public.tmp_file_lines ATTACH PARTITION public.tmp_file_lines_p20260503_060000 FOR VALUES FROM ('2026-05-03 06:00:00+00') TO ('2026-05-03 07:00:00+00');


--
-- Name: tmp_files_default; Type: TABLE ATTACH; Schema: public; Owner: -
--

ALTER TABLE ONLY public.tmp_files ATTACH PARTITION public.tmp_files_default DEFAULT;


--
-- Name: tmp_files_p20260502_220000; Type: TABLE ATTACH; Schema: public; Owner: -
--

ALTER TABLE ONLY public.tmp_files ATTACH PARTITION public.tmp_files_p20260502_220000 FOR VALUES FROM ('2026-05-02 22:00:00+00') TO ('2026-05-02 23:00:00+00');


--
-- Name: tmp_files_p20260502_230000; Type: TABLE ATTACH; Schema: public; Owner: -
--

ALTER TABLE ONLY public.tmp_files ATTACH PARTITION public.tmp_files_p20260502_230000 FOR VALUES FROM ('2026-05-02 23:00:00+00') TO ('2026-05-03 00:00:00+00');


--
-- Name: tmp_files_p20260503_000000; Type: TABLE ATTACH; Schema: public; Owner: -
--

ALTER TABLE ONLY public.tmp_files ATTACH PARTITION public.tmp_files_p20260503_000000 FOR VALUES FROM ('2026-05-03 00:00:00+00') TO ('2026-05-03 01:00:00+00');


--
-- Name: tmp_files_p20260503_010000; Type: TABLE ATTACH; Schema: public; Owner: -
--

ALTER TABLE ONLY public.tmp_files ATTACH PARTITION public.tmp_files_p20260503_010000 FOR VALUES FROM ('2026-05-03 01:00:00+00') TO ('2026-05-03 02:00:00+00');


--
-- Name: tmp_files_p20260503_020000; Type: TABLE ATTACH; Schema: public; Owner: -
--

ALTER TABLE ONLY public.tmp_files ATTACH PARTITION public.tmp_files_p20260503_020000 FOR VALUES FROM ('2026-05-03 02:00:00+00') TO ('2026-05-03 03:00:00+00');


--
-- Name: tmp_files_p20260503_030000; Type: TABLE ATTACH; Schema: public; Owner: -
--

ALTER TABLE ONLY public.tmp_files ATTACH PARTITION public.tmp_files_p20260503_030000 FOR VALUES FROM ('2026-05-03 03:00:00+00') TO ('2026-05-03 04:00:00+00');


--
-- Name: tmp_files_p20260503_040000; Type: TABLE ATTACH; Schema: public; Owner: -
--

ALTER TABLE ONLY public.tmp_files ATTACH PARTITION public.tmp_files_p20260503_040000 FOR VALUES FROM ('2026-05-03 04:00:00+00') TO ('2026-05-03 05:00:00+00');


--
-- Name: tmp_files_p20260503_050000; Type: TABLE ATTACH; Schema: public; Owner: -
--

ALTER TABLE ONLY public.tmp_files ATTACH PARTITION public.tmp_files_p20260503_050000 FOR VALUES FROM ('2026-05-03 05:00:00+00') TO ('2026-05-03 06:00:00+00');


--
-- Name: tmp_files_p20260503_060000; Type: TABLE ATTACH; Schema: public; Owner: -
--

ALTER TABLE ONLY public.tmp_files ATTACH PARTITION public.tmp_files_p20260503_060000 FOR VALUES FROM ('2026-05-03 06:00:00+00') TO ('2026-05-03 07:00:00+00');


--
-- Name: usage_daily_default; Type: TABLE ATTACH; Schema: public; Owner: -
--

ALTER TABLE ONLY public.usage_daily ATTACH PARTITION public.usage_daily_default DEFAULT;


--
-- Name: usage_daily_p20260201; Type: TABLE ATTACH; Schema: public; Owner: -
--

ALTER TABLE ONLY public.usage_daily ATTACH PARTITION public.usage_daily_p20260201 FOR VALUES FROM ('2026-02-01') TO ('2026-03-01');


--
-- Name: usage_daily_p20260301; Type: TABLE ATTACH; Schema: public; Owner: -
--

ALTER TABLE ONLY public.usage_daily ATTACH PARTITION public.usage_daily_p20260301 FOR VALUES FROM ('2026-03-01') TO ('2026-04-01');


--
-- Name: usage_daily_p20260401; Type: TABLE ATTACH; Schema: public; Owner: -
--

ALTER TABLE ONLY public.usage_daily ATTACH PARTITION public.usage_daily_p20260401 FOR VALUES FROM ('2026-04-01') TO ('2026-05-01');


--
-- Name: usage_daily_p20260501; Type: TABLE ATTACH; Schema: public; Owner: -
--

ALTER TABLE ONLY public.usage_daily ATTACH PARTITION public.usage_daily_p20260501 FOR VALUES FROM ('2026-05-01') TO ('2026-06-01');


--
-- Name: usage_daily_p20260601; Type: TABLE ATTACH; Schema: public; Owner: -
--

ALTER TABLE ONLY public.usage_daily ATTACH PARTITION public.usage_daily_p20260601 FOR VALUES FROM ('2026-06-01') TO ('2026-07-01');


--
-- Name: usage_daily_p20260701; Type: TABLE ATTACH; Schema: public; Owner: -
--

ALTER TABLE ONLY public.usage_daily ATTACH PARTITION public.usage_daily_p20260701 FOR VALUES FROM ('2026-07-01') TO ('2026-08-01');


--
-- Name: usage_daily_p20260801; Type: TABLE ATTACH; Schema: public; Owner: -
--

ALTER TABLE ONLY public.usage_daily ATTACH PARTITION public.usage_daily_p20260801 FOR VALUES FROM ('2026-08-01') TO ('2026-09-01');


--
-- Name: usage_hourly_default; Type: TABLE ATTACH; Schema: public; Owner: -
--

ALTER TABLE ONLY public.usage_hourly ATTACH PARTITION public.usage_hourly_default DEFAULT;


--
-- Name: usage_hourly_p20260430; Type: TABLE ATTACH; Schema: public; Owner: -
--

ALTER TABLE ONLY public.usage_hourly ATTACH PARTITION public.usage_hourly_p20260430 FOR VALUES FROM ('2026-04-30 00:00:00+00') TO ('2026-05-01 00:00:00+00');


--
-- Name: usage_hourly_p20260501; Type: TABLE ATTACH; Schema: public; Owner: -
--

ALTER TABLE ONLY public.usage_hourly ATTACH PARTITION public.usage_hourly_p20260501 FOR VALUES FROM ('2026-05-01 00:00:00+00') TO ('2026-05-02 00:00:00+00');


--
-- Name: usage_hourly_p20260502; Type: TABLE ATTACH; Schema: public; Owner: -
--

ALTER TABLE ONLY public.usage_hourly ATTACH PARTITION public.usage_hourly_p20260502 FOR VALUES FROM ('2026-05-02 00:00:00+00') TO ('2026-05-03 00:00:00+00');


--
-- Name: usage_hourly_p20260503; Type: TABLE ATTACH; Schema: public; Owner: -
--

ALTER TABLE ONLY public.usage_hourly ATTACH PARTITION public.usage_hourly_p20260503 FOR VALUES FROM ('2026-05-03 00:00:00+00') TO ('2026-05-04 00:00:00+00');


--
-- Name: usage_hourly_p20260504; Type: TABLE ATTACH; Schema: public; Owner: -
--

ALTER TABLE ONLY public.usage_hourly ATTACH PARTITION public.usage_hourly_p20260504 FOR VALUES FROM ('2026-05-04 00:00:00+00') TO ('2026-05-05 00:00:00+00');


--
-- Name: usage_hourly_p20260505; Type: TABLE ATTACH; Schema: public; Owner: -
--

ALTER TABLE ONLY public.usage_hourly ATTACH PARTITION public.usage_hourly_p20260505 FOR VALUES FROM ('2026-05-05 00:00:00+00') TO ('2026-05-06 00:00:00+00');


--
-- Name: usage_hourly_p20260506; Type: TABLE ATTACH; Schema: public; Owner: -
--

ALTER TABLE ONLY public.usage_hourly ATTACH PARTITION public.usage_hourly_p20260506 FOR VALUES FROM ('2026-05-06 00:00:00+00') TO ('2026-05-07 00:00:00+00');


--
-- Name: agent_tokens id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.agent_tokens ALTER COLUMN id SET DEFAULT nextval('public.agent_tokens_id_seq'::regclass);


--
-- Name: aws_accounts id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.aws_accounts ALTER COLUMN id SET DEFAULT nextval('public.accounts_id_seq'::regclass);


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
-- Name: customers id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.customers ALTER COLUMN id SET DEFAULT nextval('public.customers_id_seq'::regclass);


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
-- Name: notify_configs id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.notify_configs ALTER COLUMN id SET DEFAULT nextval('public.notify_configs_id_seq'::regclass);


--
-- Name: pagerduty_integrations id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.pagerduty_integrations ALTER COLUMN id SET DEFAULT nextval('public.pagerduty_integrations_id_seq'::regclass);


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
-- Name: aws_accounts accounts_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.aws_accounts
    ADD CONSTRAINT accounts_pkey PRIMARY KEY (id);


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
-- Name: cron_results_4h_p20260412 cron_results_4h_p20260412_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.cron_results_4h_p20260412
    ADD CONSTRAINT cron_results_4h_p20260412_pkey PRIMARY KEY (cron_watch_id, tag, bucket_start);


--
-- Name: cron_results_4h_p20260419 cron_results_4h_p20260419_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.cron_results_4h_p20260419
    ADD CONSTRAINT cron_results_4h_p20260419_pkey PRIMARY KEY (cron_watch_id, tag, bucket_start);


--
-- Name: cron_results_4h_p20260426 cron_results_4h_p20260426_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.cron_results_4h_p20260426
    ADD CONSTRAINT cron_results_4h_p20260426_pkey PRIMARY KEY (cron_watch_id, tag, bucket_start);


--
-- Name: cron_results_4h_p20260503 cron_results_4h_p20260503_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.cron_results_4h_p20260503
    ADD CONSTRAINT cron_results_4h_p20260503_pkey PRIMARY KEY (cron_watch_id, tag, bucket_start);


--
-- Name: cron_results_4h_p20260510 cron_results_4h_p20260510_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.cron_results_4h_p20260510
    ADD CONSTRAINT cron_results_4h_p20260510_pkey PRIMARY KEY (cron_watch_id, tag, bucket_start);


--
-- Name: cron_results_4h_p20260517 cron_results_4h_p20260517_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.cron_results_4h_p20260517
    ADD CONSTRAINT cron_results_4h_p20260517_pkey PRIMARY KEY (cron_watch_id, tag, bucket_start);


--
-- Name: cron_results_4h_p20260524 cron_results_4h_p20260524_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.cron_results_4h_p20260524
    ADD CONSTRAINT cron_results_4h_p20260524_pkey PRIMARY KEY (cron_watch_id, tag, bucket_start);


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
-- Name: cron_results_daily_p20260201 cron_results_daily_p20260201_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.cron_results_daily_p20260201
    ADD CONSTRAINT cron_results_daily_p20260201_pkey PRIMARY KEY (cron_watch_id, tag, bucket_start);


--
-- Name: cron_results_daily_p20260301 cron_results_daily_p20260301_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.cron_results_daily_p20260301
    ADD CONSTRAINT cron_results_daily_p20260301_pkey PRIMARY KEY (cron_watch_id, tag, bucket_start);


--
-- Name: cron_results_daily_p20260401 cron_results_daily_p20260401_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.cron_results_daily_p20260401
    ADD CONSTRAINT cron_results_daily_p20260401_pkey PRIMARY KEY (cron_watch_id, tag, bucket_start);


--
-- Name: cron_results_daily_p20260501 cron_results_daily_p20260501_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.cron_results_daily_p20260501
    ADD CONSTRAINT cron_results_daily_p20260501_pkey PRIMARY KEY (cron_watch_id, tag, bucket_start);


--
-- Name: cron_results_daily_p20260601 cron_results_daily_p20260601_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.cron_results_daily_p20260601
    ADD CONSTRAINT cron_results_daily_p20260601_pkey PRIMARY KEY (cron_watch_id, tag, bucket_start);


--
-- Name: cron_results_daily_p20260701 cron_results_daily_p20260701_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.cron_results_daily_p20260701
    ADD CONSTRAINT cron_results_daily_p20260701_pkey PRIMARY KEY (cron_watch_id, tag, bucket_start);


--
-- Name: cron_results_daily_p20260801 cron_results_daily_p20260801_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.cron_results_daily_p20260801
    ADD CONSTRAINT cron_results_daily_p20260801_pkey PRIMARY KEY (cron_watch_id, tag, bucket_start);


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
-- Name: cron_results_hourly_p20260430 cron_results_hourly_p20260430_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.cron_results_hourly_p20260430
    ADD CONSTRAINT cron_results_hourly_p20260430_pkey PRIMARY KEY (cron_watch_id, tag, bucket_start);


--
-- Name: cron_results_hourly_p20260501 cron_results_hourly_p20260501_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.cron_results_hourly_p20260501
    ADD CONSTRAINT cron_results_hourly_p20260501_pkey PRIMARY KEY (cron_watch_id, tag, bucket_start);


--
-- Name: cron_results_hourly_p20260502 cron_results_hourly_p20260502_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.cron_results_hourly_p20260502
    ADD CONSTRAINT cron_results_hourly_p20260502_pkey PRIMARY KEY (cron_watch_id, tag, bucket_start);


--
-- Name: cron_results_hourly_p20260503 cron_results_hourly_p20260503_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.cron_results_hourly_p20260503
    ADD CONSTRAINT cron_results_hourly_p20260503_pkey PRIMARY KEY (cron_watch_id, tag, bucket_start);


--
-- Name: cron_results_hourly_p20260504 cron_results_hourly_p20260504_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.cron_results_hourly_p20260504
    ADD CONSTRAINT cron_results_hourly_p20260504_pkey PRIMARY KEY (cron_watch_id, tag, bucket_start);


--
-- Name: cron_results_hourly_p20260505 cron_results_hourly_p20260505_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.cron_results_hourly_p20260505
    ADD CONSTRAINT cron_results_hourly_p20260505_pkey PRIMARY KEY (cron_watch_id, tag, bucket_start);


--
-- Name: cron_results_hourly_p20260506 cron_results_hourly_p20260506_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.cron_results_hourly_p20260506
    ADD CONSTRAINT cron_results_hourly_p20260506_pkey PRIMARY KEY (cron_watch_id, tag, bucket_start);


--
-- Name: cron_results_p20260430 cron_results_p20260430_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.cron_results_p20260430
    ADD CONSTRAINT cron_results_p20260430_pkey PRIMARY KEY (id, ran_at);


--
-- Name: cron_results_p20260501 cron_results_p20260501_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.cron_results_p20260501
    ADD CONSTRAINT cron_results_p20260501_pkey PRIMARY KEY (id, ran_at);


--
-- Name: cron_results_p20260502 cron_results_p20260502_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.cron_results_p20260502
    ADD CONSTRAINT cron_results_p20260502_pkey PRIMARY KEY (id, ran_at);


--
-- Name: cron_results_p20260503 cron_results_p20260503_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.cron_results_p20260503
    ADD CONSTRAINT cron_results_p20260503_pkey PRIMARY KEY (id, ran_at);


--
-- Name: cron_results_p20260504 cron_results_p20260504_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.cron_results_p20260504
    ADD CONSTRAINT cron_results_p20260504_pkey PRIMARY KEY (id, ran_at);


--
-- Name: cron_results_p20260505 cron_results_p20260505_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.cron_results_p20260505
    ADD CONSTRAINT cron_results_p20260505_pkey PRIMARY KEY (id, ran_at);


--
-- Name: cron_results_p20260506 cron_results_p20260506_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.cron_results_p20260506
    ADD CONSTRAINT cron_results_p20260506_pkey PRIMARY KEY (id, ran_at);


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
-- Name: customers customers_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.customers
    ADD CONSTRAINT customers_pkey PRIMARY KEY (id);


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
-- Name: notify_configs notify_configs_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.notify_configs
    ADD CONSTRAINT notify_configs_pkey PRIMARY KEY (id);


--
-- Name: pagerduty_integrations pagerduty_integrations_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.pagerduty_integrations
    ADD CONSTRAINT pagerduty_integrations_pkey PRIMARY KEY (id);


--
-- Name: saved_queries saved_queries_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.saved_queries
    ADD CONSTRAINT saved_queries_pkey PRIMARY KEY (id);


--
-- Name: shard_heartbeats shard_heartbeats_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.shard_heartbeats
    ADD CONSTRAINT shard_heartbeats_pkey PRIMARY KEY (shard_id, beat_at);


--
-- Name: shard_heartbeats_default shard_heartbeats_default_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.shard_heartbeats_default
    ADD CONSTRAINT shard_heartbeats_default_pkey PRIMARY KEY (shard_id, beat_at);


--
-- Name: shard_heartbeats_p20260503_000000 shard_heartbeats_p20260503_000000_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.shard_heartbeats_p20260503_000000
    ADD CONSTRAINT shard_heartbeats_p20260503_000000_pkey PRIMARY KEY (shard_id, beat_at);


--
-- Name: shard_heartbeats_p20260503_010000 shard_heartbeats_p20260503_010000_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.shard_heartbeats_p20260503_010000
    ADD CONSTRAINT shard_heartbeats_p20260503_010000_pkey PRIMARY KEY (shard_id, beat_at);


--
-- Name: shard_heartbeats_p20260503_020000 shard_heartbeats_p20260503_020000_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.shard_heartbeats_p20260503_020000
    ADD CONSTRAINT shard_heartbeats_p20260503_020000_pkey PRIMARY KEY (shard_id, beat_at);


--
-- Name: shard_heartbeats_p20260503_030000 shard_heartbeats_p20260503_030000_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.shard_heartbeats_p20260503_030000
    ADD CONSTRAINT shard_heartbeats_p20260503_030000_pkey PRIMARY KEY (shard_id, beat_at);


--
-- Name: shard_heartbeats_p20260503_040000 shard_heartbeats_p20260503_040000_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.shard_heartbeats_p20260503_040000
    ADD CONSTRAINT shard_heartbeats_p20260503_040000_pkey PRIMARY KEY (shard_id, beat_at);


--
-- Name: shard_records shard_records_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.shard_records
    ADD CONSTRAINT shard_records_pkey PRIMARY KEY (id);


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
-- Name: stripe_usage_reports stripe_usage_reports_customer_name_day_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.stripe_usage_reports
    ADD CONSTRAINT stripe_usage_reports_customer_name_day_key UNIQUE (customer_name, day);


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
-- Name: team_members team_members_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.team_members
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
-- Name: tmp_file_lines_p20260502_220000 tmp_file_lines_p20260502_220000_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.tmp_file_lines_p20260502_220000
    ADD CONSTRAINT tmp_file_lines_p20260502_220000_pkey PRIMARY KEY (file_id, line_num, updated_at);


--
-- Name: tmp_file_lines_p20260502_230000 tmp_file_lines_p20260502_230000_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.tmp_file_lines_p20260502_230000
    ADD CONSTRAINT tmp_file_lines_p20260502_230000_pkey PRIMARY KEY (file_id, line_num, updated_at);


--
-- Name: tmp_file_lines_p20260503_000000 tmp_file_lines_p20260503_000000_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.tmp_file_lines_p20260503_000000
    ADD CONSTRAINT tmp_file_lines_p20260503_000000_pkey PRIMARY KEY (file_id, line_num, updated_at);


--
-- Name: tmp_file_lines_p20260503_010000 tmp_file_lines_p20260503_010000_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.tmp_file_lines_p20260503_010000
    ADD CONSTRAINT tmp_file_lines_p20260503_010000_pkey PRIMARY KEY (file_id, line_num, updated_at);


--
-- Name: tmp_file_lines_p20260503_020000 tmp_file_lines_p20260503_020000_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.tmp_file_lines_p20260503_020000
    ADD CONSTRAINT tmp_file_lines_p20260503_020000_pkey PRIMARY KEY (file_id, line_num, updated_at);


--
-- Name: tmp_file_lines_p20260503_030000 tmp_file_lines_p20260503_030000_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.tmp_file_lines_p20260503_030000
    ADD CONSTRAINT tmp_file_lines_p20260503_030000_pkey PRIMARY KEY (file_id, line_num, updated_at);


--
-- Name: tmp_file_lines_p20260503_040000 tmp_file_lines_p20260503_040000_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.tmp_file_lines_p20260503_040000
    ADD CONSTRAINT tmp_file_lines_p20260503_040000_pkey PRIMARY KEY (file_id, line_num, updated_at);


--
-- Name: tmp_file_lines_p20260503_050000 tmp_file_lines_p20260503_050000_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.tmp_file_lines_p20260503_050000
    ADD CONSTRAINT tmp_file_lines_p20260503_050000_pkey PRIMARY KEY (file_id, line_num, updated_at);


--
-- Name: tmp_file_lines_p20260503_060000 tmp_file_lines_p20260503_060000_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.tmp_file_lines_p20260503_060000
    ADD CONSTRAINT tmp_file_lines_p20260503_060000_pkey PRIMARY KEY (file_id, line_num, updated_at);


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
-- Name: tmp_files_p20260502_220000 tmp_files_p20260502_220000_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.tmp_files_p20260502_220000
    ADD CONSTRAINT tmp_files_p20260502_220000_pkey PRIMARY KEY (id, updated_at);


--
-- Name: tmp_files_p20260502_230000 tmp_files_p20260502_230000_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.tmp_files_p20260502_230000
    ADD CONSTRAINT tmp_files_p20260502_230000_pkey PRIMARY KEY (id, updated_at);


--
-- Name: tmp_files_p20260503_000000 tmp_files_p20260503_000000_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.tmp_files_p20260503_000000
    ADD CONSTRAINT tmp_files_p20260503_000000_pkey PRIMARY KEY (id, updated_at);


--
-- Name: tmp_files_p20260503_010000 tmp_files_p20260503_010000_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.tmp_files_p20260503_010000
    ADD CONSTRAINT tmp_files_p20260503_010000_pkey PRIMARY KEY (id, updated_at);


--
-- Name: tmp_files_p20260503_020000 tmp_files_p20260503_020000_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.tmp_files_p20260503_020000
    ADD CONSTRAINT tmp_files_p20260503_020000_pkey PRIMARY KEY (id, updated_at);


--
-- Name: tmp_files_p20260503_030000 tmp_files_p20260503_030000_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.tmp_files_p20260503_030000
    ADD CONSTRAINT tmp_files_p20260503_030000_pkey PRIMARY KEY (id, updated_at);


--
-- Name: tmp_files_p20260503_040000 tmp_files_p20260503_040000_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.tmp_files_p20260503_040000
    ADD CONSTRAINT tmp_files_p20260503_040000_pkey PRIMARY KEY (id, updated_at);


--
-- Name: tmp_files_p20260503_050000 tmp_files_p20260503_050000_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.tmp_files_p20260503_050000
    ADD CONSTRAINT tmp_files_p20260503_050000_pkey PRIMARY KEY (id, updated_at);


--
-- Name: tmp_files_p20260503_060000 tmp_files_p20260503_060000_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.tmp_files_p20260503_060000
    ADD CONSTRAINT tmp_files_p20260503_060000_pkey PRIMARY KEY (id, updated_at);


--
-- Name: usage_daily usage_daily_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.usage_daily
    ADD CONSTRAINT usage_daily_pkey PRIMARY KEY (customer_name, day);


--
-- Name: usage_daily_default usage_daily_default_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.usage_daily_default
    ADD CONSTRAINT usage_daily_default_pkey PRIMARY KEY (customer_name, day);


--
-- Name: usage_daily_p20260201 usage_daily_p20260201_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.usage_daily_p20260201
    ADD CONSTRAINT usage_daily_p20260201_pkey PRIMARY KEY (customer_name, day);


--
-- Name: usage_daily_p20260301 usage_daily_p20260301_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.usage_daily_p20260301
    ADD CONSTRAINT usage_daily_p20260301_pkey PRIMARY KEY (customer_name, day);


--
-- Name: usage_daily_p20260401 usage_daily_p20260401_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.usage_daily_p20260401
    ADD CONSTRAINT usage_daily_p20260401_pkey PRIMARY KEY (customer_name, day);


--
-- Name: usage_daily_p20260501 usage_daily_p20260501_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.usage_daily_p20260501
    ADD CONSTRAINT usage_daily_p20260501_pkey PRIMARY KEY (customer_name, day);


--
-- Name: usage_daily_p20260601 usage_daily_p20260601_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.usage_daily_p20260601
    ADD CONSTRAINT usage_daily_p20260601_pkey PRIMARY KEY (customer_name, day);


--
-- Name: usage_daily_p20260701 usage_daily_p20260701_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.usage_daily_p20260701
    ADD CONSTRAINT usage_daily_p20260701_pkey PRIMARY KEY (customer_name, day);


--
-- Name: usage_daily_p20260801 usage_daily_p20260801_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.usage_daily_p20260801
    ADD CONSTRAINT usage_daily_p20260801_pkey PRIMARY KEY (customer_name, day);


--
-- Name: usage_hourly usage_hourly_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.usage_hourly
    ADD CONSTRAINT usage_hourly_pkey PRIMARY KEY (customer_name, hour);


--
-- Name: usage_hourly_default usage_hourly_default_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.usage_hourly_default
    ADD CONSTRAINT usage_hourly_default_pkey PRIMARY KEY (customer_name, hour);


--
-- Name: usage_hourly_p20260430 usage_hourly_p20260430_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.usage_hourly_p20260430
    ADD CONSTRAINT usage_hourly_p20260430_pkey PRIMARY KEY (customer_name, hour);


--
-- Name: usage_hourly_p20260501 usage_hourly_p20260501_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.usage_hourly_p20260501
    ADD CONSTRAINT usage_hourly_p20260501_pkey PRIMARY KEY (customer_name, hour);


--
-- Name: usage_hourly_p20260502 usage_hourly_p20260502_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.usage_hourly_p20260502
    ADD CONSTRAINT usage_hourly_p20260502_pkey PRIMARY KEY (customer_name, hour);


--
-- Name: usage_hourly_p20260503 usage_hourly_p20260503_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.usage_hourly_p20260503
    ADD CONSTRAINT usage_hourly_p20260503_pkey PRIMARY KEY (customer_name, hour);


--
-- Name: usage_hourly_p20260504 usage_hourly_p20260504_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.usage_hourly_p20260504
    ADD CONSTRAINT usage_hourly_p20260504_pkey PRIMARY KEY (customer_name, hour);


--
-- Name: usage_hourly_p20260505 usage_hourly_p20260505_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.usage_hourly_p20260505
    ADD CONSTRAINT usage_hourly_p20260505_pkey PRIMARY KEY (customer_name, hour);


--
-- Name: usage_hourly_p20260506 usage_hourly_p20260506_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.usage_hourly_p20260506
    ADD CONSTRAINT usage_hourly_p20260506_pkey PRIMARY KEY (customer_name, hour);


--
-- Name: user_notices user_notices_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.user_notices
    ADD CONSTRAINT user_notices_pkey PRIMARY KEY (user_id, notice_key);


--
-- Name: users users_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.users
    ADD CONSTRAINT users_pkey PRIMARY KEY (id);


--
-- Name: buckets_label_key; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX buckets_label_key ON public.buckets USING btree (label);


--
-- Name: idx_cron_4h_watch_tag_bucket; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_cron_4h_watch_tag_bucket ON ONLY public.cron_results_4h USING btree (cron_watch_id, tag, bucket_start DESC);


--
-- Name: cron_results_4h_default_cron_watch_id_tag_bucket_start_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX cron_results_4h_default_cron_watch_id_tag_bucket_start_idx ON public.cron_results_4h_default USING btree (cron_watch_id, tag, bucket_start DESC);


--
-- Name: cron_results_4h_p20260412_cron_watch_id_tag_bucket_start_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX cron_results_4h_p20260412_cron_watch_id_tag_bucket_start_idx ON public.cron_results_4h_p20260412 USING btree (cron_watch_id, tag, bucket_start DESC);


--
-- Name: cron_results_4h_p20260419_cron_watch_id_tag_bucket_start_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX cron_results_4h_p20260419_cron_watch_id_tag_bucket_start_idx ON public.cron_results_4h_p20260419 USING btree (cron_watch_id, tag, bucket_start DESC);


--
-- Name: cron_results_4h_p20260426_cron_watch_id_tag_bucket_start_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX cron_results_4h_p20260426_cron_watch_id_tag_bucket_start_idx ON public.cron_results_4h_p20260426 USING btree (cron_watch_id, tag, bucket_start DESC);


--
-- Name: cron_results_4h_p20260503_cron_watch_id_tag_bucket_start_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX cron_results_4h_p20260503_cron_watch_id_tag_bucket_start_idx ON public.cron_results_4h_p20260503 USING btree (cron_watch_id, tag, bucket_start DESC);


--
-- Name: cron_results_4h_p20260510_cron_watch_id_tag_bucket_start_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX cron_results_4h_p20260510_cron_watch_id_tag_bucket_start_idx ON public.cron_results_4h_p20260510 USING btree (cron_watch_id, tag, bucket_start DESC);


--
-- Name: cron_results_4h_p20260517_cron_watch_id_tag_bucket_start_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX cron_results_4h_p20260517_cron_watch_id_tag_bucket_start_idx ON public.cron_results_4h_p20260517 USING btree (cron_watch_id, tag, bucket_start DESC);


--
-- Name: cron_results_4h_p20260524_cron_watch_id_tag_bucket_start_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX cron_results_4h_p20260524_cron_watch_id_tag_bucket_start_idx ON public.cron_results_4h_p20260524 USING btree (cron_watch_id, tag, bucket_start DESC);


--
-- Name: idx_cron_daily_watch_tag_bucket; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_cron_daily_watch_tag_bucket ON ONLY public.cron_results_daily USING btree (cron_watch_id, tag, bucket_start DESC);


--
-- Name: cron_results_daily_default_cron_watch_id_tag_bucket_start_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX cron_results_daily_default_cron_watch_id_tag_bucket_start_idx ON public.cron_results_daily_default USING btree (cron_watch_id, tag, bucket_start DESC);


--
-- Name: cron_results_daily_p20260201_cron_watch_id_tag_bucket_start_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX cron_results_daily_p20260201_cron_watch_id_tag_bucket_start_idx ON public.cron_results_daily_p20260201 USING btree (cron_watch_id, tag, bucket_start DESC);


--
-- Name: cron_results_daily_p20260301_cron_watch_id_tag_bucket_start_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX cron_results_daily_p20260301_cron_watch_id_tag_bucket_start_idx ON public.cron_results_daily_p20260301 USING btree (cron_watch_id, tag, bucket_start DESC);


--
-- Name: cron_results_daily_p20260401_cron_watch_id_tag_bucket_start_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX cron_results_daily_p20260401_cron_watch_id_tag_bucket_start_idx ON public.cron_results_daily_p20260401 USING btree (cron_watch_id, tag, bucket_start DESC);


--
-- Name: cron_results_daily_p20260501_cron_watch_id_tag_bucket_start_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX cron_results_daily_p20260501_cron_watch_id_tag_bucket_start_idx ON public.cron_results_daily_p20260501 USING btree (cron_watch_id, tag, bucket_start DESC);


--
-- Name: cron_results_daily_p20260601_cron_watch_id_tag_bucket_start_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX cron_results_daily_p20260601_cron_watch_id_tag_bucket_start_idx ON public.cron_results_daily_p20260601 USING btree (cron_watch_id, tag, bucket_start DESC);


--
-- Name: cron_results_daily_p20260701_cron_watch_id_tag_bucket_start_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX cron_results_daily_p20260701_cron_watch_id_tag_bucket_start_idx ON public.cron_results_daily_p20260701 USING btree (cron_watch_id, tag, bucket_start DESC);


--
-- Name: cron_results_daily_p20260801_cron_watch_id_tag_bucket_start_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX cron_results_daily_p20260801_cron_watch_id_tag_bucket_start_idx ON public.cron_results_daily_p20260801 USING btree (cron_watch_id, tag, bucket_start DESC);


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
-- Name: cron_results_hourly_p20260430_cron_watch_id_tag_bucket_star_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX cron_results_hourly_p20260430_cron_watch_id_tag_bucket_star_idx ON public.cron_results_hourly_p20260430 USING btree (cron_watch_id, tag, bucket_start DESC);


--
-- Name: cron_results_hourly_p20260501_cron_watch_id_tag_bucket_star_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX cron_results_hourly_p20260501_cron_watch_id_tag_bucket_star_idx ON public.cron_results_hourly_p20260501 USING btree (cron_watch_id, tag, bucket_start DESC);


--
-- Name: cron_results_hourly_p20260502_cron_watch_id_tag_bucket_star_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX cron_results_hourly_p20260502_cron_watch_id_tag_bucket_star_idx ON public.cron_results_hourly_p20260502 USING btree (cron_watch_id, tag, bucket_start DESC);


--
-- Name: cron_results_hourly_p20260503_cron_watch_id_tag_bucket_star_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX cron_results_hourly_p20260503_cron_watch_id_tag_bucket_star_idx ON public.cron_results_hourly_p20260503 USING btree (cron_watch_id, tag, bucket_start DESC);


--
-- Name: cron_results_hourly_p20260504_cron_watch_id_tag_bucket_star_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX cron_results_hourly_p20260504_cron_watch_id_tag_bucket_star_idx ON public.cron_results_hourly_p20260504 USING btree (cron_watch_id, tag, bucket_start DESC);


--
-- Name: cron_results_hourly_p20260505_cron_watch_id_tag_bucket_star_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX cron_results_hourly_p20260505_cron_watch_id_tag_bucket_star_idx ON public.cron_results_hourly_p20260505 USING btree (cron_watch_id, tag, bucket_start DESC);


--
-- Name: cron_results_hourly_p20260506_cron_watch_id_tag_bucket_star_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX cron_results_hourly_p20260506_cron_watch_id_tag_bucket_star_idx ON public.cron_results_hourly_p20260506 USING btree (cron_watch_id, tag, bucket_start DESC);


--
-- Name: cron_results_p20260430_cron_watch_id_tag_ran_at_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX cron_results_p20260430_cron_watch_id_tag_ran_at_idx ON public.cron_results_p20260430 USING btree (cron_watch_id, tag, ran_at DESC);


--
-- Name: cron_results_p20260430_deleted_at_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX cron_results_p20260430_deleted_at_idx ON public.cron_results_p20260430 USING btree (deleted_at);


--
-- Name: cron_results_p20260501_cron_watch_id_tag_ran_at_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX cron_results_p20260501_cron_watch_id_tag_ran_at_idx ON public.cron_results_p20260501 USING btree (cron_watch_id, tag, ran_at DESC);


--
-- Name: cron_results_p20260501_deleted_at_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX cron_results_p20260501_deleted_at_idx ON public.cron_results_p20260501 USING btree (deleted_at);


--
-- Name: cron_results_p20260502_cron_watch_id_tag_ran_at_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX cron_results_p20260502_cron_watch_id_tag_ran_at_idx ON public.cron_results_p20260502 USING btree (cron_watch_id, tag, ran_at DESC);


--
-- Name: cron_results_p20260502_deleted_at_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX cron_results_p20260502_deleted_at_idx ON public.cron_results_p20260502 USING btree (deleted_at);


--
-- Name: cron_results_p20260503_cron_watch_id_tag_ran_at_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX cron_results_p20260503_cron_watch_id_tag_ran_at_idx ON public.cron_results_p20260503 USING btree (cron_watch_id, tag, ran_at DESC);


--
-- Name: cron_results_p20260503_deleted_at_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX cron_results_p20260503_deleted_at_idx ON public.cron_results_p20260503 USING btree (deleted_at);


--
-- Name: cron_results_p20260504_cron_watch_id_tag_ran_at_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX cron_results_p20260504_cron_watch_id_tag_ran_at_idx ON public.cron_results_p20260504 USING btree (cron_watch_id, tag, ran_at DESC);


--
-- Name: cron_results_p20260504_deleted_at_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX cron_results_p20260504_deleted_at_idx ON public.cron_results_p20260504 USING btree (deleted_at);


--
-- Name: cron_results_p20260505_cron_watch_id_tag_ran_at_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX cron_results_p20260505_cron_watch_id_tag_ran_at_idx ON public.cron_results_p20260505 USING btree (cron_watch_id, tag, ran_at DESC);


--
-- Name: cron_results_p20260505_deleted_at_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX cron_results_p20260505_deleted_at_idx ON public.cron_results_p20260505 USING btree (deleted_at);


--
-- Name: cron_results_p20260506_cron_watch_id_tag_ran_at_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX cron_results_p20260506_cron_watch_id_tag_ran_at_idx ON public.cron_results_p20260506 USING btree (cron_watch_id, tag, ran_at DESC);


--
-- Name: cron_results_p20260506_deleted_at_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX cron_results_p20260506_deleted_at_idx ON public.cron_results_p20260506 USING btree (deleted_at);


--
-- Name: idx_accounts_deleted_at; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_accounts_deleted_at ON public.aws_accounts USING btree (deleted_at);


--
-- Name: idx_agent_tokens_deleted_at; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_agent_tokens_deleted_at ON public.agent_tokens USING btree (deleted_at);


--
-- Name: idx_agent_tokens_token_hash; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX idx_agent_tokens_token_hash ON public.agent_tokens USING btree (token_hash);


--
-- Name: idx_cron_customer_name; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX idx_cron_customer_name ON public.cron_watches USING btree (customer_id, name);


--
-- Name: idx_cron_watches_deleted_at; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_cron_watches_deleted_at ON public.cron_watches USING btree (deleted_at);


--
-- Name: idx_customers_deleted_at; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_customers_deleted_at ON public.customers USING btree (deleted_at);


--
-- Name: idx_customers_name; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX idx_customers_name ON public.customers USING btree (name);


--
-- Name: idx_cwh_content_hash; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_cwh_content_hash ON public.cron_watch_history USING btree (watch_id, content_hash);


--
-- Name: idx_cwh_customer_name; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_cwh_customer_name ON public.cron_watch_history USING btree (customer_id, name, captured_at DESC);


--
-- Name: idx_cwh_watch_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_cwh_watch_id ON public.cron_watch_history USING btree (watch_id, captured_at DESC);


--
-- Name: idx_dashboards_customer_name; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX idx_dashboards_customer_name ON public.dashboards USING btree (customer_id, name) WHERE (deleted_at IS NULL);


--
-- Name: idx_dashboards_deleted_at; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_dashboards_deleted_at ON public.dashboards USING btree (deleted_at);


--
-- Name: idx_device_codes_device_code; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX idx_device_codes_device_code ON public.device_codes USING btree (device_code);


--
-- Name: idx_device_codes_user_code; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX idx_device_codes_user_code ON public.device_codes USING btree (user_code);


--
-- Name: idx_func_customer_name; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX idx_func_customer_name ON public.shell_functions USING btree (customer_id, name);


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
-- Name: idx_magic_links_token; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX idx_magic_links_token ON public.magic_links USING btree (token);


--
-- Name: idx_namespace_permissions_deleted_at; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_namespace_permissions_deleted_at ON public.namespace_permissions USING btree (deleted_at);


--
-- Name: idx_namespaces_deleted_at; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_namespaces_deleted_at ON public.namespaces USING btree (deleted_at);


--
-- Name: idx_notify_configs_customer_id; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX idx_notify_configs_customer_id ON public.notify_configs USING btree (customer_id);


--
-- Name: idx_notify_configs_deleted_at; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_notify_configs_deleted_at ON public.notify_configs USING btree (deleted_at);


--
-- Name: idx_ns_app; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX idx_ns_app ON public.log_groups USING btree (namespace_id, app);


--
-- Name: idx_ns_customer_name; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX idx_ns_customer_name ON public.namespaces USING btree (customer_id, name);


--
-- Name: idx_nsperm_user_ns; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX idx_nsperm_user_ns ON public.namespace_permissions USING btree (user_id, namespace_id);


--
-- Name: idx_pd_integrations_customer_name; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX idx_pd_integrations_customer_name ON public.pagerduty_integrations USING btree (customer_id, name) WHERE (deleted_at IS NULL);


--
-- Name: idx_pd_integrations_deleted_at; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_pd_integrations_deleted_at ON public.pagerduty_integrations USING btree (deleted_at);


--
-- Name: idx_saved_queries_user_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_saved_queries_user_id ON public.saved_queries USING btree (user_id);


--
-- Name: idx_sfh_content_hash; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_sfh_content_hash ON public.shell_function_history USING btree (function_id, content_hash);


--
-- Name: idx_sfh_customer_name; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_sfh_customer_name ON public.shell_function_history USING btree (customer_id, name, captured_at DESC);


--
-- Name: idx_sfh_function_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_sfh_function_id ON public.shell_function_history USING btree (function_id, captured_at DESC);


--
-- Name: idx_shell_functions_deleted_at; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_shell_functions_deleted_at ON public.shell_functions USING btree (deleted_at);


--
-- Name: idx_stripe_usage_reports_day; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_stripe_usage_reports_day ON public.stripe_usage_reports USING btree (day);


--
-- Name: idx_subscriptions_customer_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_subscriptions_customer_id ON public.subscriptions USING btree (customer_id);


--
-- Name: idx_team_members_customer; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_team_members_customer ON public.team_members USING btree (customer_id);


--
-- Name: idx_team_members_user_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_team_members_user_id ON public.team_members USING btree (user_id);


--
-- Name: idx_tmp_file_lines_file_line; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_tmp_file_lines_file_line ON ONLY public.tmp_file_lines USING btree (file_id, line_num);


--
-- Name: idx_tmp_files_customer_path; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_tmp_files_customer_path ON ONLY public.tmp_files USING btree (customer_id, path);


--
-- Name: idx_users_email; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX idx_users_email ON public.users USING btree (email);


--
-- Name: tmp_file_lines_default_file_id_line_num_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX tmp_file_lines_default_file_id_line_num_idx ON public.tmp_file_lines_default USING btree (file_id, line_num);


--
-- Name: tmp_file_lines_p20260502_220000_file_id_line_num_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX tmp_file_lines_p20260502_220000_file_id_line_num_idx ON public.tmp_file_lines_p20260502_220000 USING btree (file_id, line_num);


--
-- Name: tmp_file_lines_p20260502_230000_file_id_line_num_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX tmp_file_lines_p20260502_230000_file_id_line_num_idx ON public.tmp_file_lines_p20260502_230000 USING btree (file_id, line_num);


--
-- Name: tmp_file_lines_p20260503_000000_file_id_line_num_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX tmp_file_lines_p20260503_000000_file_id_line_num_idx ON public.tmp_file_lines_p20260503_000000 USING btree (file_id, line_num);


--
-- Name: tmp_file_lines_p20260503_010000_file_id_line_num_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX tmp_file_lines_p20260503_010000_file_id_line_num_idx ON public.tmp_file_lines_p20260503_010000 USING btree (file_id, line_num);


--
-- Name: tmp_file_lines_p20260503_020000_file_id_line_num_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX tmp_file_lines_p20260503_020000_file_id_line_num_idx ON public.tmp_file_lines_p20260503_020000 USING btree (file_id, line_num);


--
-- Name: tmp_file_lines_p20260503_030000_file_id_line_num_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX tmp_file_lines_p20260503_030000_file_id_line_num_idx ON public.tmp_file_lines_p20260503_030000 USING btree (file_id, line_num);


--
-- Name: tmp_file_lines_p20260503_040000_file_id_line_num_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX tmp_file_lines_p20260503_040000_file_id_line_num_idx ON public.tmp_file_lines_p20260503_040000 USING btree (file_id, line_num);


--
-- Name: tmp_file_lines_p20260503_050000_file_id_line_num_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX tmp_file_lines_p20260503_050000_file_id_line_num_idx ON public.tmp_file_lines_p20260503_050000 USING btree (file_id, line_num);


--
-- Name: tmp_file_lines_p20260503_060000_file_id_line_num_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX tmp_file_lines_p20260503_060000_file_id_line_num_idx ON public.tmp_file_lines_p20260503_060000 USING btree (file_id, line_num);


--
-- Name: tmp_files_default_customer_id_path_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX tmp_files_default_customer_id_path_idx ON public.tmp_files_default USING btree (customer_id, path);


--
-- Name: tmp_files_p20260502_220000_customer_id_path_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX tmp_files_p20260502_220000_customer_id_path_idx ON public.tmp_files_p20260502_220000 USING btree (customer_id, path);


--
-- Name: tmp_files_p20260502_230000_customer_id_path_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX tmp_files_p20260502_230000_customer_id_path_idx ON public.tmp_files_p20260502_230000 USING btree (customer_id, path);


--
-- Name: tmp_files_p20260503_000000_customer_id_path_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX tmp_files_p20260503_000000_customer_id_path_idx ON public.tmp_files_p20260503_000000 USING btree (customer_id, path);


--
-- Name: tmp_files_p20260503_010000_customer_id_path_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX tmp_files_p20260503_010000_customer_id_path_idx ON public.tmp_files_p20260503_010000 USING btree (customer_id, path);


--
-- Name: tmp_files_p20260503_020000_customer_id_path_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX tmp_files_p20260503_020000_customer_id_path_idx ON public.tmp_files_p20260503_020000 USING btree (customer_id, path);


--
-- Name: tmp_files_p20260503_030000_customer_id_path_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX tmp_files_p20260503_030000_customer_id_path_idx ON public.tmp_files_p20260503_030000 USING btree (customer_id, path);


--
-- Name: tmp_files_p20260503_040000_customer_id_path_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX tmp_files_p20260503_040000_customer_id_path_idx ON public.tmp_files_p20260503_040000 USING btree (customer_id, path);


--
-- Name: tmp_files_p20260503_050000_customer_id_path_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX tmp_files_p20260503_050000_customer_id_path_idx ON public.tmp_files_p20260503_050000 USING btree (customer_id, path);


--
-- Name: tmp_files_p20260503_060000_customer_id_path_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX tmp_files_p20260503_060000_customer_id_path_idx ON public.tmp_files_p20260503_060000 USING btree (customer_id, path);


--
-- Name: cron_results_4h_default_cron_watch_id_tag_bucket_start_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.idx_cron_4h_watch_tag_bucket ATTACH PARTITION public.cron_results_4h_default_cron_watch_id_tag_bucket_start_idx;


--
-- Name: cron_results_4h_default_pkey; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.cron_results_4h_pkey ATTACH PARTITION public.cron_results_4h_default_pkey;


--
-- Name: cron_results_4h_p20260412_cron_watch_id_tag_bucket_start_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.idx_cron_4h_watch_tag_bucket ATTACH PARTITION public.cron_results_4h_p20260412_cron_watch_id_tag_bucket_start_idx;


--
-- Name: cron_results_4h_p20260412_pkey; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.cron_results_4h_pkey ATTACH PARTITION public.cron_results_4h_p20260412_pkey;


--
-- Name: cron_results_4h_p20260419_cron_watch_id_tag_bucket_start_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.idx_cron_4h_watch_tag_bucket ATTACH PARTITION public.cron_results_4h_p20260419_cron_watch_id_tag_bucket_start_idx;


--
-- Name: cron_results_4h_p20260419_pkey; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.cron_results_4h_pkey ATTACH PARTITION public.cron_results_4h_p20260419_pkey;


--
-- Name: cron_results_4h_p20260426_cron_watch_id_tag_bucket_start_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.idx_cron_4h_watch_tag_bucket ATTACH PARTITION public.cron_results_4h_p20260426_cron_watch_id_tag_bucket_start_idx;


--
-- Name: cron_results_4h_p20260426_pkey; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.cron_results_4h_pkey ATTACH PARTITION public.cron_results_4h_p20260426_pkey;


--
-- Name: cron_results_4h_p20260503_cron_watch_id_tag_bucket_start_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.idx_cron_4h_watch_tag_bucket ATTACH PARTITION public.cron_results_4h_p20260503_cron_watch_id_tag_bucket_start_idx;


--
-- Name: cron_results_4h_p20260503_pkey; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.cron_results_4h_pkey ATTACH PARTITION public.cron_results_4h_p20260503_pkey;


--
-- Name: cron_results_4h_p20260510_cron_watch_id_tag_bucket_start_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.idx_cron_4h_watch_tag_bucket ATTACH PARTITION public.cron_results_4h_p20260510_cron_watch_id_tag_bucket_start_idx;


--
-- Name: cron_results_4h_p20260510_pkey; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.cron_results_4h_pkey ATTACH PARTITION public.cron_results_4h_p20260510_pkey;


--
-- Name: cron_results_4h_p20260517_cron_watch_id_tag_bucket_start_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.idx_cron_4h_watch_tag_bucket ATTACH PARTITION public.cron_results_4h_p20260517_cron_watch_id_tag_bucket_start_idx;


--
-- Name: cron_results_4h_p20260517_pkey; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.cron_results_4h_pkey ATTACH PARTITION public.cron_results_4h_p20260517_pkey;


--
-- Name: cron_results_4h_p20260524_cron_watch_id_tag_bucket_start_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.idx_cron_4h_watch_tag_bucket ATTACH PARTITION public.cron_results_4h_p20260524_cron_watch_id_tag_bucket_start_idx;


--
-- Name: cron_results_4h_p20260524_pkey; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.cron_results_4h_pkey ATTACH PARTITION public.cron_results_4h_p20260524_pkey;


--
-- Name: cron_results_daily_default_cron_watch_id_tag_bucket_start_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.idx_cron_daily_watch_tag_bucket ATTACH PARTITION public.cron_results_daily_default_cron_watch_id_tag_bucket_start_idx;


--
-- Name: cron_results_daily_default_pkey; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.cron_results_daily_pkey ATTACH PARTITION public.cron_results_daily_default_pkey;


--
-- Name: cron_results_daily_p20260201_cron_watch_id_tag_bucket_start_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.idx_cron_daily_watch_tag_bucket ATTACH PARTITION public.cron_results_daily_p20260201_cron_watch_id_tag_bucket_start_idx;


--
-- Name: cron_results_daily_p20260201_pkey; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.cron_results_daily_pkey ATTACH PARTITION public.cron_results_daily_p20260201_pkey;


--
-- Name: cron_results_daily_p20260301_cron_watch_id_tag_bucket_start_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.idx_cron_daily_watch_tag_bucket ATTACH PARTITION public.cron_results_daily_p20260301_cron_watch_id_tag_bucket_start_idx;


--
-- Name: cron_results_daily_p20260301_pkey; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.cron_results_daily_pkey ATTACH PARTITION public.cron_results_daily_p20260301_pkey;


--
-- Name: cron_results_daily_p20260401_cron_watch_id_tag_bucket_start_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.idx_cron_daily_watch_tag_bucket ATTACH PARTITION public.cron_results_daily_p20260401_cron_watch_id_tag_bucket_start_idx;


--
-- Name: cron_results_daily_p20260401_pkey; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.cron_results_daily_pkey ATTACH PARTITION public.cron_results_daily_p20260401_pkey;


--
-- Name: cron_results_daily_p20260501_cron_watch_id_tag_bucket_start_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.idx_cron_daily_watch_tag_bucket ATTACH PARTITION public.cron_results_daily_p20260501_cron_watch_id_tag_bucket_start_idx;


--
-- Name: cron_results_daily_p20260501_pkey; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.cron_results_daily_pkey ATTACH PARTITION public.cron_results_daily_p20260501_pkey;


--
-- Name: cron_results_daily_p20260601_cron_watch_id_tag_bucket_start_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.idx_cron_daily_watch_tag_bucket ATTACH PARTITION public.cron_results_daily_p20260601_cron_watch_id_tag_bucket_start_idx;


--
-- Name: cron_results_daily_p20260601_pkey; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.cron_results_daily_pkey ATTACH PARTITION public.cron_results_daily_p20260601_pkey;


--
-- Name: cron_results_daily_p20260701_cron_watch_id_tag_bucket_start_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.idx_cron_daily_watch_tag_bucket ATTACH PARTITION public.cron_results_daily_p20260701_cron_watch_id_tag_bucket_start_idx;


--
-- Name: cron_results_daily_p20260701_pkey; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.cron_results_daily_pkey ATTACH PARTITION public.cron_results_daily_p20260701_pkey;


--
-- Name: cron_results_daily_p20260801_cron_watch_id_tag_bucket_start_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.idx_cron_daily_watch_tag_bucket ATTACH PARTITION public.cron_results_daily_p20260801_cron_watch_id_tag_bucket_start_idx;


--
-- Name: cron_results_daily_p20260801_pkey; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.cron_results_daily_pkey ATTACH PARTITION public.cron_results_daily_p20260801_pkey;


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
-- Name: cron_results_hourly_p20260430_cron_watch_id_tag_bucket_star_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.idx_cron_hourly_watch_tag_bucket ATTACH PARTITION public.cron_results_hourly_p20260430_cron_watch_id_tag_bucket_star_idx;


--
-- Name: cron_results_hourly_p20260430_pkey; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.cron_results_hourly_pkey ATTACH PARTITION public.cron_results_hourly_p20260430_pkey;


--
-- Name: cron_results_hourly_p20260501_cron_watch_id_tag_bucket_star_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.idx_cron_hourly_watch_tag_bucket ATTACH PARTITION public.cron_results_hourly_p20260501_cron_watch_id_tag_bucket_star_idx;


--
-- Name: cron_results_hourly_p20260501_pkey; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.cron_results_hourly_pkey ATTACH PARTITION public.cron_results_hourly_p20260501_pkey;


--
-- Name: cron_results_hourly_p20260502_cron_watch_id_tag_bucket_star_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.idx_cron_hourly_watch_tag_bucket ATTACH PARTITION public.cron_results_hourly_p20260502_cron_watch_id_tag_bucket_star_idx;


--
-- Name: cron_results_hourly_p20260502_pkey; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.cron_results_hourly_pkey ATTACH PARTITION public.cron_results_hourly_p20260502_pkey;


--
-- Name: cron_results_hourly_p20260503_cron_watch_id_tag_bucket_star_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.idx_cron_hourly_watch_tag_bucket ATTACH PARTITION public.cron_results_hourly_p20260503_cron_watch_id_tag_bucket_star_idx;


--
-- Name: cron_results_hourly_p20260503_pkey; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.cron_results_hourly_pkey ATTACH PARTITION public.cron_results_hourly_p20260503_pkey;


--
-- Name: cron_results_hourly_p20260504_cron_watch_id_tag_bucket_star_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.idx_cron_hourly_watch_tag_bucket ATTACH PARTITION public.cron_results_hourly_p20260504_cron_watch_id_tag_bucket_star_idx;


--
-- Name: cron_results_hourly_p20260504_pkey; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.cron_results_hourly_pkey ATTACH PARTITION public.cron_results_hourly_p20260504_pkey;


--
-- Name: cron_results_hourly_p20260505_cron_watch_id_tag_bucket_star_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.idx_cron_hourly_watch_tag_bucket ATTACH PARTITION public.cron_results_hourly_p20260505_cron_watch_id_tag_bucket_star_idx;


--
-- Name: cron_results_hourly_p20260505_pkey; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.cron_results_hourly_pkey ATTACH PARTITION public.cron_results_hourly_p20260505_pkey;


--
-- Name: cron_results_hourly_p20260506_cron_watch_id_tag_bucket_star_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.idx_cron_hourly_watch_tag_bucket ATTACH PARTITION public.cron_results_hourly_p20260506_cron_watch_id_tag_bucket_star_idx;


--
-- Name: cron_results_hourly_p20260506_pkey; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.cron_results_hourly_pkey ATTACH PARTITION public.cron_results_hourly_p20260506_pkey;


--
-- Name: cron_results_p20260430_cron_watch_id_tag_ran_at_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.idx_cron_results_watch_tag_ran_at ATTACH PARTITION public.cron_results_p20260430_cron_watch_id_tag_ran_at_idx;


--
-- Name: cron_results_p20260430_deleted_at_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.idx_cron_results_deleted_at ATTACH PARTITION public.cron_results_p20260430_deleted_at_idx;


--
-- Name: cron_results_p20260430_pkey; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.cron_results_pkey ATTACH PARTITION public.cron_results_p20260430_pkey;


--
-- Name: cron_results_p20260501_cron_watch_id_tag_ran_at_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.idx_cron_results_watch_tag_ran_at ATTACH PARTITION public.cron_results_p20260501_cron_watch_id_tag_ran_at_idx;


--
-- Name: cron_results_p20260501_deleted_at_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.idx_cron_results_deleted_at ATTACH PARTITION public.cron_results_p20260501_deleted_at_idx;


--
-- Name: cron_results_p20260501_pkey; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.cron_results_pkey ATTACH PARTITION public.cron_results_p20260501_pkey;


--
-- Name: cron_results_p20260502_cron_watch_id_tag_ran_at_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.idx_cron_results_watch_tag_ran_at ATTACH PARTITION public.cron_results_p20260502_cron_watch_id_tag_ran_at_idx;


--
-- Name: cron_results_p20260502_deleted_at_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.idx_cron_results_deleted_at ATTACH PARTITION public.cron_results_p20260502_deleted_at_idx;


--
-- Name: cron_results_p20260502_pkey; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.cron_results_pkey ATTACH PARTITION public.cron_results_p20260502_pkey;


--
-- Name: cron_results_p20260503_cron_watch_id_tag_ran_at_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.idx_cron_results_watch_tag_ran_at ATTACH PARTITION public.cron_results_p20260503_cron_watch_id_tag_ran_at_idx;


--
-- Name: cron_results_p20260503_deleted_at_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.idx_cron_results_deleted_at ATTACH PARTITION public.cron_results_p20260503_deleted_at_idx;


--
-- Name: cron_results_p20260503_pkey; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.cron_results_pkey ATTACH PARTITION public.cron_results_p20260503_pkey;


--
-- Name: cron_results_p20260504_cron_watch_id_tag_ran_at_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.idx_cron_results_watch_tag_ran_at ATTACH PARTITION public.cron_results_p20260504_cron_watch_id_tag_ran_at_idx;


--
-- Name: cron_results_p20260504_deleted_at_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.idx_cron_results_deleted_at ATTACH PARTITION public.cron_results_p20260504_deleted_at_idx;


--
-- Name: cron_results_p20260504_pkey; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.cron_results_pkey ATTACH PARTITION public.cron_results_p20260504_pkey;


--
-- Name: cron_results_p20260505_cron_watch_id_tag_ran_at_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.idx_cron_results_watch_tag_ran_at ATTACH PARTITION public.cron_results_p20260505_cron_watch_id_tag_ran_at_idx;


--
-- Name: cron_results_p20260505_deleted_at_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.idx_cron_results_deleted_at ATTACH PARTITION public.cron_results_p20260505_deleted_at_idx;


--
-- Name: cron_results_p20260505_pkey; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.cron_results_pkey ATTACH PARTITION public.cron_results_p20260505_pkey;


--
-- Name: cron_results_p20260506_cron_watch_id_tag_ran_at_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.idx_cron_results_watch_tag_ran_at ATTACH PARTITION public.cron_results_p20260506_cron_watch_id_tag_ran_at_idx;


--
-- Name: cron_results_p20260506_deleted_at_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.idx_cron_results_deleted_at ATTACH PARTITION public.cron_results_p20260506_deleted_at_idx;


--
-- Name: cron_results_p20260506_pkey; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.cron_results_pkey ATTACH PARTITION public.cron_results_p20260506_pkey;


--
-- Name: shard_heartbeats_default_pkey; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.shard_heartbeats_pkey ATTACH PARTITION public.shard_heartbeats_default_pkey;


--
-- Name: shard_heartbeats_p20260503_000000_pkey; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.shard_heartbeats_pkey ATTACH PARTITION public.shard_heartbeats_p20260503_000000_pkey;


--
-- Name: shard_heartbeats_p20260503_010000_pkey; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.shard_heartbeats_pkey ATTACH PARTITION public.shard_heartbeats_p20260503_010000_pkey;


--
-- Name: shard_heartbeats_p20260503_020000_pkey; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.shard_heartbeats_pkey ATTACH PARTITION public.shard_heartbeats_p20260503_020000_pkey;


--
-- Name: shard_heartbeats_p20260503_030000_pkey; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.shard_heartbeats_pkey ATTACH PARTITION public.shard_heartbeats_p20260503_030000_pkey;


--
-- Name: shard_heartbeats_p20260503_040000_pkey; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.shard_heartbeats_pkey ATTACH PARTITION public.shard_heartbeats_p20260503_040000_pkey;


--
-- Name: tmp_file_lines_default_file_id_line_num_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.idx_tmp_file_lines_file_line ATTACH PARTITION public.tmp_file_lines_default_file_id_line_num_idx;


--
-- Name: tmp_file_lines_default_pkey; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.tmp_file_lines_pkey ATTACH PARTITION public.tmp_file_lines_default_pkey;


--
-- Name: tmp_file_lines_p20260502_220000_file_id_line_num_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.idx_tmp_file_lines_file_line ATTACH PARTITION public.tmp_file_lines_p20260502_220000_file_id_line_num_idx;


--
-- Name: tmp_file_lines_p20260502_220000_pkey; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.tmp_file_lines_pkey ATTACH PARTITION public.tmp_file_lines_p20260502_220000_pkey;


--
-- Name: tmp_file_lines_p20260502_230000_file_id_line_num_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.idx_tmp_file_lines_file_line ATTACH PARTITION public.tmp_file_lines_p20260502_230000_file_id_line_num_idx;


--
-- Name: tmp_file_lines_p20260502_230000_pkey; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.tmp_file_lines_pkey ATTACH PARTITION public.tmp_file_lines_p20260502_230000_pkey;


--
-- Name: tmp_file_lines_p20260503_000000_file_id_line_num_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.idx_tmp_file_lines_file_line ATTACH PARTITION public.tmp_file_lines_p20260503_000000_file_id_line_num_idx;


--
-- Name: tmp_file_lines_p20260503_000000_pkey; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.tmp_file_lines_pkey ATTACH PARTITION public.tmp_file_lines_p20260503_000000_pkey;


--
-- Name: tmp_file_lines_p20260503_010000_file_id_line_num_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.idx_tmp_file_lines_file_line ATTACH PARTITION public.tmp_file_lines_p20260503_010000_file_id_line_num_idx;


--
-- Name: tmp_file_lines_p20260503_010000_pkey; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.tmp_file_lines_pkey ATTACH PARTITION public.tmp_file_lines_p20260503_010000_pkey;


--
-- Name: tmp_file_lines_p20260503_020000_file_id_line_num_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.idx_tmp_file_lines_file_line ATTACH PARTITION public.tmp_file_lines_p20260503_020000_file_id_line_num_idx;


--
-- Name: tmp_file_lines_p20260503_020000_pkey; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.tmp_file_lines_pkey ATTACH PARTITION public.tmp_file_lines_p20260503_020000_pkey;


--
-- Name: tmp_file_lines_p20260503_030000_file_id_line_num_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.idx_tmp_file_lines_file_line ATTACH PARTITION public.tmp_file_lines_p20260503_030000_file_id_line_num_idx;


--
-- Name: tmp_file_lines_p20260503_030000_pkey; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.tmp_file_lines_pkey ATTACH PARTITION public.tmp_file_lines_p20260503_030000_pkey;


--
-- Name: tmp_file_lines_p20260503_040000_file_id_line_num_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.idx_tmp_file_lines_file_line ATTACH PARTITION public.tmp_file_lines_p20260503_040000_file_id_line_num_idx;


--
-- Name: tmp_file_lines_p20260503_040000_pkey; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.tmp_file_lines_pkey ATTACH PARTITION public.tmp_file_lines_p20260503_040000_pkey;


--
-- Name: tmp_file_lines_p20260503_050000_file_id_line_num_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.idx_tmp_file_lines_file_line ATTACH PARTITION public.tmp_file_lines_p20260503_050000_file_id_line_num_idx;


--
-- Name: tmp_file_lines_p20260503_050000_pkey; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.tmp_file_lines_pkey ATTACH PARTITION public.tmp_file_lines_p20260503_050000_pkey;


--
-- Name: tmp_file_lines_p20260503_060000_file_id_line_num_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.idx_tmp_file_lines_file_line ATTACH PARTITION public.tmp_file_lines_p20260503_060000_file_id_line_num_idx;


--
-- Name: tmp_file_lines_p20260503_060000_pkey; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.tmp_file_lines_pkey ATTACH PARTITION public.tmp_file_lines_p20260503_060000_pkey;


--
-- Name: tmp_files_default_customer_id_path_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.idx_tmp_files_customer_path ATTACH PARTITION public.tmp_files_default_customer_id_path_idx;


--
-- Name: tmp_files_default_pkey; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.tmp_files_pkey ATTACH PARTITION public.tmp_files_default_pkey;


--
-- Name: tmp_files_p20260502_220000_customer_id_path_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.idx_tmp_files_customer_path ATTACH PARTITION public.tmp_files_p20260502_220000_customer_id_path_idx;


--
-- Name: tmp_files_p20260502_220000_pkey; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.tmp_files_pkey ATTACH PARTITION public.tmp_files_p20260502_220000_pkey;


--
-- Name: tmp_files_p20260502_230000_customer_id_path_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.idx_tmp_files_customer_path ATTACH PARTITION public.tmp_files_p20260502_230000_customer_id_path_idx;


--
-- Name: tmp_files_p20260502_230000_pkey; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.tmp_files_pkey ATTACH PARTITION public.tmp_files_p20260502_230000_pkey;


--
-- Name: tmp_files_p20260503_000000_customer_id_path_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.idx_tmp_files_customer_path ATTACH PARTITION public.tmp_files_p20260503_000000_customer_id_path_idx;


--
-- Name: tmp_files_p20260503_000000_pkey; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.tmp_files_pkey ATTACH PARTITION public.tmp_files_p20260503_000000_pkey;


--
-- Name: tmp_files_p20260503_010000_customer_id_path_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.idx_tmp_files_customer_path ATTACH PARTITION public.tmp_files_p20260503_010000_customer_id_path_idx;


--
-- Name: tmp_files_p20260503_010000_pkey; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.tmp_files_pkey ATTACH PARTITION public.tmp_files_p20260503_010000_pkey;


--
-- Name: tmp_files_p20260503_020000_customer_id_path_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.idx_tmp_files_customer_path ATTACH PARTITION public.tmp_files_p20260503_020000_customer_id_path_idx;


--
-- Name: tmp_files_p20260503_020000_pkey; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.tmp_files_pkey ATTACH PARTITION public.tmp_files_p20260503_020000_pkey;


--
-- Name: tmp_files_p20260503_030000_customer_id_path_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.idx_tmp_files_customer_path ATTACH PARTITION public.tmp_files_p20260503_030000_customer_id_path_idx;


--
-- Name: tmp_files_p20260503_030000_pkey; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.tmp_files_pkey ATTACH PARTITION public.tmp_files_p20260503_030000_pkey;


--
-- Name: tmp_files_p20260503_040000_customer_id_path_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.idx_tmp_files_customer_path ATTACH PARTITION public.tmp_files_p20260503_040000_customer_id_path_idx;


--
-- Name: tmp_files_p20260503_040000_pkey; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.tmp_files_pkey ATTACH PARTITION public.tmp_files_p20260503_040000_pkey;


--
-- Name: tmp_files_p20260503_050000_customer_id_path_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.idx_tmp_files_customer_path ATTACH PARTITION public.tmp_files_p20260503_050000_customer_id_path_idx;


--
-- Name: tmp_files_p20260503_050000_pkey; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.tmp_files_pkey ATTACH PARTITION public.tmp_files_p20260503_050000_pkey;


--
-- Name: tmp_files_p20260503_060000_customer_id_path_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.idx_tmp_files_customer_path ATTACH PARTITION public.tmp_files_p20260503_060000_customer_id_path_idx;


--
-- Name: tmp_files_p20260503_060000_pkey; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.tmp_files_pkey ATTACH PARTITION public.tmp_files_p20260503_060000_pkey;


--
-- Name: usage_daily_default_pkey; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.usage_daily_pkey ATTACH PARTITION public.usage_daily_default_pkey;


--
-- Name: usage_daily_p20260201_pkey; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.usage_daily_pkey ATTACH PARTITION public.usage_daily_p20260201_pkey;


--
-- Name: usage_daily_p20260301_pkey; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.usage_daily_pkey ATTACH PARTITION public.usage_daily_p20260301_pkey;


--
-- Name: usage_daily_p20260401_pkey; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.usage_daily_pkey ATTACH PARTITION public.usage_daily_p20260401_pkey;


--
-- Name: usage_daily_p20260501_pkey; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.usage_daily_pkey ATTACH PARTITION public.usage_daily_p20260501_pkey;


--
-- Name: usage_daily_p20260601_pkey; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.usage_daily_pkey ATTACH PARTITION public.usage_daily_p20260601_pkey;


--
-- Name: usage_daily_p20260701_pkey; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.usage_daily_pkey ATTACH PARTITION public.usage_daily_p20260701_pkey;


--
-- Name: usage_daily_p20260801_pkey; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.usage_daily_pkey ATTACH PARTITION public.usage_daily_p20260801_pkey;


--
-- Name: usage_hourly_default_pkey; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.usage_hourly_pkey ATTACH PARTITION public.usage_hourly_default_pkey;


--
-- Name: usage_hourly_p20260430_pkey; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.usage_hourly_pkey ATTACH PARTITION public.usage_hourly_p20260430_pkey;


--
-- Name: usage_hourly_p20260501_pkey; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.usage_hourly_pkey ATTACH PARTITION public.usage_hourly_p20260501_pkey;


--
-- Name: usage_hourly_p20260502_pkey; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.usage_hourly_pkey ATTACH PARTITION public.usage_hourly_p20260502_pkey;


--
-- Name: usage_hourly_p20260503_pkey; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.usage_hourly_pkey ATTACH PARTITION public.usage_hourly_p20260503_pkey;


--
-- Name: usage_hourly_p20260504_pkey; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.usage_hourly_pkey ATTACH PARTITION public.usage_hourly_p20260504_pkey;


--
-- Name: usage_hourly_p20260505_pkey; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.usage_hourly_pkey ATTACH PARTITION public.usage_hourly_p20260505_pkey;


--
-- Name: usage_hourly_p20260506_pkey; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.usage_hourly_pkey ATTACH PARTITION public.usage_hourly_p20260506_pkey;


--
-- Name: cron_watches capture_cron_watch_history_trg; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER capture_cron_watch_history_trg BEFORE UPDATE OF schedule, pipeline ON public.cron_watches FOR EACH ROW EXECUTE FUNCTION public.capture_cron_watch_history();


--
-- Name: shell_functions capture_shell_function_history_trg; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER capture_shell_function_history_trg BEFORE UPDATE OF source, language ON public.shell_functions FOR EACH ROW EXECUTE FUNCTION public.capture_shell_function_history();


--
-- Name: cron_watch_history cron_watch_history_predecessor_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.cron_watch_history
    ADD CONSTRAINT cron_watch_history_predecessor_id_fkey FOREIGN KEY (predecessor_id) REFERENCES public.cron_watch_history(id) ON DELETE SET NULL;


--
-- Name: log_groups fk_accounts_log_groups; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.log_groups
    ADD CONSTRAINT fk_accounts_log_groups FOREIGN KEY (aws_account_id) REFERENCES public.aws_accounts(id) ON DELETE CASCADE;


--
-- Name: agent_tokens fk_agent_tokens_namespace; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.agent_tokens
    ADD CONSTRAINT fk_agent_tokens_namespace FOREIGN KEY (namespace_id) REFERENCES public.namespaces(id) ON DELETE CASCADE;


--
-- Name: aws_accounts fk_customers_accounts; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.aws_accounts
    ADD CONSTRAINT fk_customers_accounts FOREIGN KEY (customer_id) REFERENCES public.customers(id) ON DELETE CASCADE;


--
-- Name: agent_tokens fk_customers_agent_tokens; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.agent_tokens
    ADD CONSTRAINT fk_customers_agent_tokens FOREIGN KEY (customer_id) REFERENCES public.customers(id) ON DELETE CASCADE;


--
-- Name: namespaces fk_customers_namespaces; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.namespaces
    ADD CONSTRAINT fk_customers_namespaces FOREIGN KEY (customer_id) REFERENCES public.customers(id) ON DELETE CASCADE;


--
-- Name: team_members fk_team_members_customer; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.team_members
    ADD CONSTRAINT fk_team_members_customer FOREIGN KEY (customer_id) REFERENCES public.customers(id) ON DELETE CASCADE;


--
-- Name: log_groups log_groups_namespace_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.log_groups
    ADD CONSTRAINT log_groups_namespace_id_fkey FOREIGN KEY (namespace_id) REFERENCES public.namespaces(id) ON DELETE CASCADE;


--
-- Name: pagerduty_integrations pagerduty_integrations_customer_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.pagerduty_integrations
    ADD CONSTRAINT pagerduty_integrations_customer_id_fkey FOREIGN KEY (customer_id) REFERENCES public.customers(id) ON DELETE CASCADE;


--
-- Name: shard_records shard_records_bucket_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.shard_records
    ADD CONSTRAINT shard_records_bucket_id_fkey FOREIGN KEY (bucket_id) REFERENCES public.buckets(id);


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
-- Name: users users_last_active_customer_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.users
    ADD CONSTRAINT users_last_active_customer_id_fkey FOREIGN KEY (last_active_customer_id) REFERENCES public.customers(id) ON DELETE SET NULL;


--
-- PostgreSQL database dump complete
--

\unrestrict T866diS4GuVmoS1KA0jiwxObCb4g0xFIgTlGuWmA4fg3vbHfqYZAUpmPmV6aixW

--
-- PostgreSQL database dump
--

\restrict 2xy8I7IWhTWHcqHDBg75Vzx8NbRi0hTXWKNAKe9MhsVh0VxxrC11JGLL2Z4KPJg

-- Dumped from database version 16.13 (Debian 16.13-1.pgdg13+1)
-- Dumped by pg_dump version 16.13 (Debian 16.13-1.pgdg13+1)

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


--
-- Name: goose_db_version_id_seq; Type: SEQUENCE SET; Schema: public; Owner: -
--

SELECT pg_catalog.setval('public.goose_db_version_id_seq', 42, true);


--
-- PostgreSQL database dump complete
--

\unrestrict 2xy8I7IWhTWHcqHDBg75Vzx8NbRi0hTXWKNAKe9MhsVh0VxxrC11JGLL2Z4KPJg

