-- +goose Up
-- Switch usage_hourly → usage_daily rollup to the idempotent pattern
-- already in use for cron_results (migration 026):
--
--   * The rollup no longer deletes from usage_hourly. pg_partman's
--     existing 7-day retention drops aged hourly partitions.
--   * A stored procedure re-aggregates the last 3 days every hour
--     via ON CONFLICT DO UPDATE, so the daily table is always
--     correct for any day with at least one rollup tick behind it.
--
-- The previous DELETE-after-INSERT job was non-atomic — two separate
-- transactions, a reader between them double-counted the rolled
-- rows. Billing correctness matters now that `docs/plans/stripe.md`
-- calls for a daily Stripe usage report against usage_daily.

-- +goose StatementBegin
CREATE OR REPLACE PROCEDURE public.rollup_usage_daily()
LANGUAGE SQL AS $$
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
-- +goose StatementEnd

-- Replace the old nightly DELETE-based job with the new hourly
-- idempotent rollup. `cron.unschedule` is safe when the job exists;
-- we don't guard it because the migration is one-shot.

-- +goose StatementBegin
SELECT cron.unschedule('usage-rollup');
-- +goose StatementEnd

-- +goose StatementBegin
SELECT cron.schedule('usage-rollup-daily', '0 * * * *',
    $$CALL public.rollup_usage_daily()$$);
-- +goose StatementEnd

-- +goose Down

-- +goose StatementBegin
SELECT cron.unschedule('usage-rollup-daily');
-- +goose StatementEnd

DROP PROCEDURE IF EXISTS public.rollup_usage_daily;

-- +goose StatementBegin
SELECT cron.schedule('usage-rollup', '0 2 * * *', $$
  INSERT INTO usage_daily (customer_name, day, bytes_ingested)
  SELECT customer_name, date_trunc('day', hour)::date, SUM(bytes_ingested)
  FROM usage_hourly
  WHERE hour < NOW() - INTERVAL '7 days'
  GROUP BY customer_name, date_trunc('day', hour)::date
  ON CONFLICT (customer_name, day) DO UPDATE
  SET bytes_ingested = EXCLUDED.bytes_ingested;

  DELETE FROM usage_hourly WHERE hour < NOW() - INTERVAL '7 days';
$$);
-- +goose StatementEnd
