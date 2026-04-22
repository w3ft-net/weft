-- +goose Up
-- Partition cron_results by ran_at and add a numeric value column so alert
-- aggregations (avg/min/max/count over a duration window) can run as SQL
-- against Postgres instead of being computed in Go. No watches exist yet,
-- so we drop and recreate rather than migrating rows.

DROP TABLE IF EXISTS public.cron_results CASCADE;
DROP SEQUENCE IF EXISTS public.cron_results_id_seq;

CREATE SEQUENCE public.cron_results_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

CREATE TABLE public.cron_results (
    id            BIGINT NOT NULL DEFAULT nextval('public.cron_results_id_seq'),
    created_at    TIMESTAMPTZ,
    updated_at    TIMESTAMPTZ,
    deleted_at    TIMESTAMPTZ,
    cron_watch_id BIGINT NOT NULL,
    ran_at        TIMESTAMPTZ NOT NULL,
    stdout        TEXT,
    stderr        TEXT,
    exit_code     BIGINT,
    duration_ms   BIGINT,
    value         DOUBLE PRECISION,
    PRIMARY KEY (id, ran_at)
) PARTITION BY RANGE (ran_at);

ALTER SEQUENCE public.cron_results_id_seq OWNED BY public.cron_results.id;

CREATE INDEX idx_cron_results_watch_ran_at
    ON public.cron_results (cron_watch_id, ran_at DESC);
CREATE INDEX idx_cron_results_deleted_at
    ON public.cron_results (deleted_at);

SELECT partman.create_parent(
    p_parent_table := 'public.cron_results',
    p_control := 'ran_at',
    p_interval := '1 day',
    p_premake := 3
);
UPDATE partman.part_config
SET retention = '30 days', retention_keep_table = false
WHERE parent_table = 'public.cron_results';
