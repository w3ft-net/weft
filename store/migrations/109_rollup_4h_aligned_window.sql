-- +goose Up
-- rollup_cron_4h read hourly buckets from (current hour - 12h), a window edge
-- that falls mid-bucket three hours in four. Each run replaces a 4h bucket with
-- whatever the window still holds of it, so as the window slid past a bucket's
-- first hours the stored value shrank to its last hour or two, and
-- rollup_cron_daily (which sums the 4h tier) inherited the loss. Counts and sums
-- over any window that reads the 4h or daily tier came out at a quarter to a
-- third of the truth.
--
-- Start the window on a bucket edge, using the expression that assigns buckets,
-- so a bucket is either read whole or not at all.

-- +goose StatementBegin
CREATE OR REPLACE PROCEDURE public.rollup_cron_4h()
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
    WHERE bucket_start >= (
              SELECT date_trunc('day', w.t)
                       + INTERVAL '4 hours' * FLOOR(EXTRACT(hour FROM w.t) / 4)
              FROM (SELECT date_trunc('hour', now()) - INTERVAL '12 hours' AS t) w
          )
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

-- Rebuild the 4h buckets the old window damaged, from the hourly tier. Only
-- buckets whose four hours are all still in the hourly tier are rebuilt; older
-- ones have lost their source rows and stay as they are.
INSERT INTO public.cron_results_4h
    (cron_watch_id, tag, bucket_start, count, sum, sum_sq, min, max)
SELECT
    h.cron_watch_id,
    h.tag,
    date_trunc('day', h.bucket_start)
      + INTERVAL '4 hours' * FLOOR(EXTRACT(hour FROM h.bucket_start) / 4),
    SUM(h.count),
    SUM(h.sum),
    SUM(h.sum_sq),
    MIN(h.min),
    MAX(h.max)
FROM public.cron_results_hourly h
WHERE h.bucket_start >= (
          SELECT date_trunc('day', m.t)
                   + INTERVAL '4 hours' * CEIL(EXTRACT(hour FROM m.t) / 4.0)
          FROM (SELECT MIN(bucket_start) AS t FROM public.cron_results_hourly) m
      )
  AND h.bucket_start < date_trunc('hour', now()) - INTERVAL '1 hour'
GROUP BY h.cron_watch_id, h.tag,
         date_trunc('day', h.bucket_start)
           + INTERVAL '4 hours' * FLOOR(EXTRACT(hour FROM h.bucket_start) / 4)
ON CONFLICT (cron_watch_id, tag, bucket_start) DO UPDATE SET
    count  = EXCLUDED.count,
    sum    = EXCLUDED.sum,
    sum_sq = EXCLUDED.sum_sq,
    min    = EXCLUDED.min,
    max    = EXCLUDED.max;

-- And the daily buckets built from them: whole days from the first day the
-- rebuilt 4h range covers completely, up to (not including) today.
INSERT INTO public.cron_results_daily
    (cron_watch_id, tag, bucket_start, count, sum, sum_sq, min, max)
SELECT
    f.cron_watch_id,
    f.tag,
    date_trunc('day', f.bucket_start),
    SUM(f.count),
    SUM(f.sum),
    SUM(f.sum_sq),
    MIN(f.min),
    MAX(f.max)
FROM public.cron_results_4h f
WHERE f.bucket_start >= (
          SELECT CASE WHEN date_trunc('day', m.t) = m.t
                      THEN m.t
                      ELSE date_trunc('day', m.t) + INTERVAL '1 day' END
          FROM (SELECT MIN(bucket_start) AS t FROM public.cron_results_hourly) m
      )
  AND f.bucket_start < date_trunc('day', now())
GROUP BY f.cron_watch_id, f.tag, date_trunc('day', f.bucket_start)
ON CONFLICT (cron_watch_id, tag, bucket_start) DO UPDATE SET
    count  = EXCLUDED.count,
    sum    = EXCLUDED.sum,
    sum_sq = EXCLUDED.sum_sq,
    min    = EXCLUDED.min,
    max    = EXCLUDED.max;

-- +goose Down
-- Restores the previous window. The rebuilt rows are correct and stay.
-- +goose StatementBegin
CREATE OR REPLACE PROCEDURE public.rollup_cron_4h()
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
-- +goose StatementEnd
