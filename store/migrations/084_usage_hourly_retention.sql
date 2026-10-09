-- +goose Up
-- usage_hourly had no retention: it has only a default partition and is not
-- registered with partman, so nothing ever dropped rows (175k rows back to
-- April, ~9k/day). The hourly rollup already re-rolls the last 3 days into
-- usage_daily, so anything older than 14 days is long since folded in.
-- Pruning inside rollup_usage_daily() reuses its existing hourly pg_cron job
-- and runs in every environment, including ones without pg_cron.
--
-- The partman templates for usage_hourly/usage_daily were orphaned: neither
-- table was ever registered with create_parent.

-- +goose StatementBegin
CREATE OR REPLACE PROCEDURE public.rollup_usage_daily()
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
-- +goose StatementEnd

-- The rollup and the prune both filter on hour alone, which the (org_id, hour)
-- index cannot serve, so each scanned the whole table. Rows are insert-only and
-- arrive in time order, so BRIN is tiny and costs inserts almost nothing.
CREATE INDEX idx_usage_hourly_hour_brin ON public.usage_hourly USING brin (hour);

DROP TABLE IF EXISTS partman.template_public_usage_hourly;
DROP TABLE IF EXISTS partman.template_public_usage_daily;

-- +goose Down
DROP INDEX IF EXISTS public.idx_usage_hourly_hour_brin;
-- +goose StatementBegin
CREATE OR REPLACE PROCEDURE public.rollup_usage_daily()
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
$$;
-- +goose StatementEnd
