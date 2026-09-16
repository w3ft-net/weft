-- +goose Up
-- usage_hourly moves from "one row per (customer, hour), updated in
-- place via ON CONFLICT DO UPDATE" to append-only: every flush inserts
-- a new row, and readers SUM across however many rows landed for that
-- hour. This makes concurrent flushes from multiple coordinator
-- instances trivially safe (plain INSERT, no shared mutable row to
-- race on) instead of relying on Postgres's additive UPSERT being
-- correct under concurrency -- which it already was, but append-only
-- removes the need to reason about it at all, and gives us a durable
-- per-flush record for debugging instead of a single collapsed number.
--
-- rollup_usage_daily() and QueryUsage already SUM(bytes_ingested)
-- GROUP BY customer/hour-or-day -- neither assumes one row per hour,
-- so this needs no changes on the read side. See coordinator/coordinator.go
-- (flushUsage) and coordinator/usage_stats.go (Drain) for the write side.
--
-- Dropping the PK requires the parent table's constraint drop to cascade
-- to usage_hourly_default's own partition index; Postgres does this
-- automatically for a partitioned index dropped at the parent. The
-- replacement index keeps the same (customer_name, hour) column order
-- so the existing range-scan queries in QueryUsage don't regress.

ALTER TABLE public.usage_hourly DROP CONSTRAINT IF EXISTS usage_hourly_pkey;

CREATE INDEX IF NOT EXISTS idx_usage_hourly_customer_hour
  ON public.usage_hourly (customer_name, hour);

-- +goose Down
-- Best-effort only: this fails if append-only writes have already
-- produced more than one row for some (customer_name, hour) pair, since
-- a primary key can't be re-added over duplicates. Manually collapse
-- duplicates (sum into one row per key) before rolling back if needed.

DROP INDEX IF EXISTS public.idx_usage_hourly_customer_hour;

ALTER TABLE public.usage_hourly
  ADD CONSTRAINT usage_hourly_pkey PRIMARY KEY (customer_name, hour);
