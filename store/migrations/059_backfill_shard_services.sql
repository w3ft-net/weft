-- +goose Up
-- Backfill shard_services (migration 058) for shards christened before
-- christen-time stamping existed. Same logic as Store.StampShardServices,
-- run for every existing shard at once: a shard is stamped with its
-- bucket's services, read from the canonical registry (log_groups),
-- resolving a NULL log_groups.bucket_id to the default bucket.
--
-- Without this, routing can't read shard_services exclusively — queries
-- against data on pre-stamping shards would find nothing. Idempotent
-- (ON CONFLICT DO NOTHING): shards stamped at christen are unaffected.
INSERT INTO shard_services (shard_id, namespace_id, service)
SELECT sr.id, lg.namespace_id, lg.app
FROM shard_records sr
JOIN log_groups lg
  ON COALESCE(lg.bucket_id, (SELECT id FROM buckets WHERE label = 'default')) = sr.bucket_id
WHERE lg.deleted_at IS NULL
ON CONFLICT (shard_id, namespace_id, service) DO NOTHING;

-- +goose Down
-- No clean inverse (can't tell backfilled rows from christen-stamped
-- ones). shard_services is dropped by 058's Down.
SELECT 1;
