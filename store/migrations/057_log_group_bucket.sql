-- +goose Up
-- A service's bucket assignment lives on its log_groups row, not in a
-- separate table. log_groups already has unique(namespace_id, app) —
-- exactly one row per service — so the bucket is just an attribute of
-- that row, and "one bucket per service" stays automatic.
--
-- NULL = the default bucket (the day-1 home); a non-NULL value is a
-- service explicitly moved to a dedicated bucket (faster shard
-- rotation / larger ASG). This needs no backfill — existing rows are
-- NULL = default — and "services in bucket X" is a plain WHERE rather
-- than a JSON-array scan. Services that have ingested but aren't yet
-- sampled have no log_groups row at all and are implicitly default.
--
-- Buckets group services for shard redundancy/sizing and are the
-- mechanism that stamps shards with their services at christen; that
-- stamp is permanent, so this column describes only the *current*
-- assignment.
ALTER TABLE log_groups
  ADD COLUMN bucket_id bigint REFERENCES buckets(id);

-- +goose Down
ALTER TABLE log_groups DROP COLUMN bucket_id;
