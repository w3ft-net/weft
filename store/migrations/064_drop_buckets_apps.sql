-- +goose Up
-- Drop buckets.apps. Buckets don't serve services — shards do. A service's
-- bucket assignment lives in log_groups.bucket_id (NULL = default bucket);
-- routing reads shard_services (per-shard membership). The namespace-less
-- buckets.apps JSON list was a parallel, namespace-blind source of truth
-- that nothing routes against, and the disabled Split feature was its only
-- consumer (now removed pending docs/plans/bucket-split-policy.md).
ALTER TABLE buckets DROP COLUMN IF EXISTS apps;

-- +goose Down
-- Restore as a nullable text column (the original was NOT NULL with the Go
-- layer always supplying '[]'; on rollback the column is unused, so a
-- plain nullable add is enough to let old binaries boot).
ALTER TABLE buckets ADD COLUMN IF NOT EXISTS apps TEXT;
