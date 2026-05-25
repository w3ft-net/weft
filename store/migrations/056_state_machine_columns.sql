-- +goose Up
-- State-machine persistence columns for the bucket + shard FSM
-- rework. See docs/plans/asg-as-api-stateless.md.
--
-- buckets.state holds the bucket FSM's current state
-- (STASIS/WARMING/ROTATING/ARCHIVING/SCALING_DOWN). NOT NULL with
-- default STASIS so existing rows hydrate cleanly.
--
-- buckets.cascade_started_at tracks the wall-clock entry into
-- WARMING so WARMING.OnTick can extend outgoing-shard WindowEnds if
-- the cascade stalls. NULL outside of an in-flight cascade.
--
-- shard_records gains five columns:
--   - ingest_failures, ingest_unhealthy: persist the per-shard
--     ingest-health state that lived in-memory only and was lost on
--     restart (the `Suspended` boolean rename).
--   - last_heartbeat_at: lets the post-restart freshness check
--     distinguish "never observed" from "observed but stale"; today's
--     in-memory zero-value defaults to "fresh" on boot which masks
--     dead shards for one tick window.
--   - launch_attempts: per-shard launch retry budget tracked across
--     ticks. Bounded retries; alerts beyond.
--   - az: persisted AZ identity for the shard. Recovery launches
--     (SUSPENDED.OnEntry) read this directly instead of inferring
--     from the volume tag. Volume tag stays as a backstop.
--   - stripe_index: the bucket-assigned stripe slot for this shard.
--     Incoming shards in a cascade inherit the outgoing shard's
--     stripe_index so the AZ layout carries across windows.
--
-- idx_shard_records_bucket_window indexes the (bucket_id,
-- window_start) lookups the bucket SM does every tick to find
-- incoming/outgoing shards.

ALTER TABLE buckets
  ADD COLUMN IF NOT EXISTS state              TEXT NOT NULL DEFAULT 'STASIS',
  ADD COLUMN IF NOT EXISTS cascade_started_at TIMESTAMPTZ NULL;

ALTER TABLE shard_records
  ADD COLUMN IF NOT EXISTS ingest_failures    INT     NOT NULL DEFAULT 0,
  ADD COLUMN IF NOT EXISTS ingest_unhealthy   BOOLEAN NOT NULL DEFAULT FALSE,
  ADD COLUMN IF NOT EXISTS last_heartbeat_at  TIMESTAMPTZ NULL,
  ADD COLUMN IF NOT EXISTS launch_attempts    INT     NOT NULL DEFAULT 0,
  ADD COLUMN IF NOT EXISTS az                 TEXT    NULL,
  ADD COLUMN IF NOT EXISTS stripe_index       INT     NULL;

CREATE INDEX IF NOT EXISTS idx_shard_records_bucket_window
  ON shard_records (bucket_id, window_start);

-- +goose Down
DROP INDEX IF EXISTS idx_shard_records_bucket_window;

ALTER TABLE shard_records
  DROP COLUMN IF EXISTS stripe_index,
  DROP COLUMN IF EXISTS az,
  DROP COLUMN IF EXISTS launch_attempts,
  DROP COLUMN IF EXISTS last_heartbeat_at,
  DROP COLUMN IF EXISTS ingest_unhealthy,
  DROP COLUMN IF EXISTS ingest_failures;

ALTER TABLE buckets
  DROP COLUMN IF EXISTS cascade_started_at,
  DROP COLUMN IF EXISTS state;
