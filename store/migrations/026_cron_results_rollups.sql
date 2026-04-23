-- +goose Up
-- Rollup pyramid for cron_results. Raw data shrinks from 30d → 7d.
-- Three coarser tiers — hourly (14d), 4-hourly (35d), daily (400d) —
-- keep historical trend data queryable at decreasing resolution.
-- Per-plan retention is enforced at query time via LogRetention() in
-- store/user.go; on-disk retention is global, honoring the pyramid.
-- See docs/plans/retention-enforcement.md for the path to real
-- storage-side per-plan enforcement.
--
-- Rollup row shape is shared across all three tiers:
--
--   (cron_watch_id, tag, bucket_start, count, sum, sum_sq, min, max)
--
-- count + sum + sum_sq preserve avg, sum, and stddev exactly through
-- any amount of further aggregation. min and max chain trivially.

-- ── 1. Shrink raw retention: 30d → 7d ─────────────────────────────

UPDATE partman.part_config
SET retention = '7 days'
WHERE parent_table = 'public.cron_results';

-- ── 2. Hourly tier (7–14 days) ────────────────────────────────────

CREATE TABLE public.cron_results_hourly (
    cron_watch_id BIGINT           NOT NULL,
    tag           TEXT             NOT NULL DEFAULT '',
    bucket_start  TIMESTAMPTZ      NOT NULL,
    count         BIGINT           NOT NULL,
    sum           DOUBLE PRECISION NOT NULL,
    sum_sq        DOUBLE PRECISION NOT NULL,
    min           DOUBLE PRECISION NOT NULL,
    max           DOUBLE PRECISION NOT NULL,
    PRIMARY KEY (cron_watch_id, tag, bucket_start)
) PARTITION BY RANGE (bucket_start);

CREATE INDEX idx_cron_hourly_watch_tag_bucket
    ON public.cron_results_hourly (cron_watch_id, tag, bucket_start DESC);

SELECT partman.create_parent(
    p_parent_table := 'public.cron_results_hourly',
    p_control := 'bucket_start',
    p_interval := '1 day',
    p_premake := 3
);
UPDATE partman.part_config
SET retention = '14 days', retention_keep_table = false
WHERE parent_table = 'public.cron_results_hourly';

-- ── 3. Four-hourly tier (14–35 days) ──────────────────────────────

CREATE TABLE public.cron_results_4h (
    cron_watch_id BIGINT           NOT NULL,
    tag           TEXT             NOT NULL DEFAULT '',
    bucket_start  TIMESTAMPTZ      NOT NULL,
    count         BIGINT           NOT NULL,
    sum           DOUBLE PRECISION NOT NULL,
    sum_sq        DOUBLE PRECISION NOT NULL,
    min           DOUBLE PRECISION NOT NULL,
    max           DOUBLE PRECISION NOT NULL,
    PRIMARY KEY (cron_watch_id, tag, bucket_start)
) PARTITION BY RANGE (bucket_start);

CREATE INDEX idx_cron_4h_watch_tag_bucket
    ON public.cron_results_4h (cron_watch_id, tag, bucket_start DESC);

SELECT partman.create_parent(
    p_parent_table := 'public.cron_results_4h',
    p_control := 'bucket_start',
    p_interval := '1 week',
    p_premake := 3
);
UPDATE partman.part_config
SET retention = '35 days', retention_keep_table = false
WHERE parent_table = 'public.cron_results_4h';

-- ── 4. Daily tier (35–400 days) ───────────────────────────────────

CREATE TABLE public.cron_results_daily (
    cron_watch_id BIGINT           NOT NULL,
    tag           TEXT             NOT NULL DEFAULT '',
    bucket_start  TIMESTAMPTZ      NOT NULL,
    count         BIGINT           NOT NULL,
    sum           DOUBLE PRECISION NOT NULL,
    sum_sq        DOUBLE PRECISION NOT NULL,
    min           DOUBLE PRECISION NOT NULL,
    max           DOUBLE PRECISION NOT NULL,
    PRIMARY KEY (cron_watch_id, tag, bucket_start)
) PARTITION BY RANGE (bucket_start);

CREATE INDEX idx_cron_daily_watch_tag_bucket
    ON public.cron_results_daily (cron_watch_id, tag, bucket_start DESC);

SELECT partman.create_parent(
    p_parent_table := 'public.cron_results_daily',
    p_control := 'bucket_start',
    p_interval := '1 month',
    p_premake := 3
);
UPDATE partman.part_config
SET retention = '400 days', retention_keep_table = false
WHERE parent_table = 'public.cron_results_daily';

-- ── 5. Rollup procedures ──────────────────────────────────────────
-- Each proc rolls up the last few closed buckets from the finer tier.
-- Rollups are idempotent: ON CONFLICT DO UPDATE means a late-arriving
-- raw row gets folded into its already-written rollup without
-- double-counting.

-- +goose StatementBegin
CREATE OR REPLACE PROCEDURE public.rollup_cron_hourly()
LANGUAGE SQL AS $$
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
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE PROCEDURE public.rollup_cron_4h()
LANGUAGE SQL AS $$
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
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE PROCEDURE public.rollup_cron_daily()
LANGUAGE SQL AS $$
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
-- +goose StatementEnd

-- ── 6. pg_cron schedules ──────────────────────────────────────────
-- Cadence is "finer than the tier's granularity" so late-arriving raw
-- rows always have a chance to land in the right bucket before its
-- rollup proc closes it off.

-- +goose StatementBegin
SELECT cron.schedule('cron-rollup-hourly', '*/5 * * * *',
    $$CALL public.rollup_cron_hourly()$$);
-- +goose StatementEnd

-- +goose StatementBegin
SELECT cron.schedule('cron-rollup-4h', '15 * * * *',
    $$CALL public.rollup_cron_4h()$$);
-- +goose StatementEnd

-- +goose StatementBegin
SELECT cron.schedule('cron-rollup-daily', '30 * * * *',
    $$CALL public.rollup_cron_daily()$$);
-- +goose StatementEnd

-- ── 7. Union view for tier-agnostic reads ─────────────────────────
-- Callers MUST filter by granularity — the view is UNION ALL, not a
-- coalesce. Picking a tier lives in the Go query router; this view
-- just gives one SQL name for "all five shapes of metric data".
-- Raw rows are projected into the rollup shape with count=1 so SELECT
-- AVG(sum/count) etc. works uniformly across tiers.

CREATE OR REPLACE VIEW public.cron_results_all AS
SELECT 'raw'::text    AS granularity,
       cron_watch_id,
       tag,
       ran_at          AS bucket_start,
       1::bigint       AS count,
       value           AS sum,
       value * value   AS sum_sq,
       value           AS min,
       value           AS max
FROM public.cron_results
WHERE value IS NOT NULL
UNION ALL
SELECT 'hourly'::text, cron_watch_id, tag, bucket_start, count, sum, sum_sq, min, max
FROM public.cron_results_hourly
UNION ALL
SELECT '4h'::text, cron_watch_id, tag, bucket_start, count, sum, sum_sq, min, max
FROM public.cron_results_4h
UNION ALL
SELECT 'daily'::text, cron_watch_id, tag, bucket_start, count, sum, sum_sq, min, max
FROM public.cron_results_daily;

-- +goose Down

DROP VIEW IF EXISTS public.cron_results_all;

-- +goose StatementBegin
SELECT cron.unschedule('cron-rollup-daily');
-- +goose StatementEnd
-- +goose StatementBegin
SELECT cron.unschedule('cron-rollup-4h');
-- +goose StatementEnd
-- +goose StatementBegin
SELECT cron.unschedule('cron-rollup-hourly');
-- +goose StatementEnd

DROP PROCEDURE IF EXISTS public.rollup_cron_daily;
DROP PROCEDURE IF EXISTS public.rollup_cron_4h;
DROP PROCEDURE IF EXISTS public.rollup_cron_hourly;

-- +goose StatementBegin
SELECT partman.undo_partition('public.cron_results_daily',
    p_keep_table := false, p_target_table := NULL);
-- +goose StatementEnd
DROP TABLE IF EXISTS public.cron_results_daily;

-- +goose StatementBegin
SELECT partman.undo_partition('public.cron_results_4h',
    p_keep_table := false, p_target_table := NULL);
-- +goose StatementEnd
DROP TABLE IF EXISTS public.cron_results_4h;

-- +goose StatementBegin
SELECT partman.undo_partition('public.cron_results_hourly',
    p_keep_table := false, p_target_table := NULL);
-- +goose StatementEnd
DROP TABLE IF EXISTS public.cron_results_hourly;

UPDATE partman.part_config
SET retention = '30 days'
WHERE parent_table = 'public.cron_results';
