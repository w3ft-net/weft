-- Usage metering tables with pg_partman-managed partitioning.
-- Run once after creating the weft database.
--
-- Prerequisites:
--   CREATE EXTENSION IF NOT EXISTS pg_partman;
--   CREATE EXTENSION IF NOT EXISTS pg_cron;

-- Hourly ingestion counters, partitioned by day (7-day retention).
CREATE TABLE IF NOT EXISTS usage_hourly (
    customer_name TEXT NOT NULL,
    hour          TIMESTAMPTZ NOT NULL,
    bytes_ingested BIGINT NOT NULL DEFAULT 0,
    PRIMARY KEY (customer_name, hour)
) PARTITION BY RANGE (hour);

-- Daily rollup, partitioned by month (13-month retention).
CREATE TABLE IF NOT EXISTS usage_daily (
    customer_name TEXT NOT NULL,
    day           DATE NOT NULL,
    bytes_ingested BIGINT NOT NULL DEFAULT 0,
    PRIMARY KEY (customer_name, day)
) PARTITION BY RANGE (day);

-- pg_partman: auto-create and drop partitions.
SELECT public.create_parent(
    p_parent_table := 'public.usage_hourly',
    p_control := 'hour',
    p_interval := '1 day',
    p_premake := 3
);
UPDATE public.part_config
SET retention = '7 days', retention_keep_table = false
WHERE parent_table = 'public.usage_hourly';

SELECT public.create_parent(
    p_parent_table := 'public.usage_daily',
    p_control := 'day',
    p_interval := '1 month',
    p_premake := 3
);
UPDATE public.part_config
SET retention = '13 months', retention_keep_table = false
WHERE parent_table = 'public.usage_daily';

-- pg_cron: run pg_partman maintenance every 30 minutes
-- (creates new partitions, drops expired ones)
SELECT cron.schedule('partman-maintenance', '*/30 * * * *',
    $$CALL public.run_maintenance_proc()$$);

-- pg_cron: daily rollup of completed hourly data into daily aggregates.
-- Runs at 02:00 UTC. Rolls up hours older than 7 days, then deletes them.
-- (The rollup preserves data in usage_daily before pg_partman drops the
-- hourly partition.)
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
