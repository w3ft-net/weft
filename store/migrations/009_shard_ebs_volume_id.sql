-- +goose Up
-- Cache the shard's EBS volume id alongside the shard record so recovery
-- doesn't have to DescribeVolumes every time an instance dies. The tag
-- on the volume itself (tag:shard_id=<name>) remains the source of truth;
-- this column is a convenience cache the coordinator keeps in sync on
-- every attach/detach. See docs/plans/ebs-shard-volumes.md → S2.
ALTER TABLE shard_records ADD COLUMN IF NOT EXISTS ebs_volume_id TEXT;

-- +goose Down
ALTER TABLE shard_records DROP COLUMN IF EXISTS ebs_volume_id;
