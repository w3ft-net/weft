-- +goose Up
-- Drop shard_records.apps. A shard's services now live in the normalized
-- shard_services table (migration 058), stamped at christen and appended
-- on live service-add; the registry loads each ShardEntry's in-memory
-- service membership from there. The denormalized JSON apps column has no
-- remaining readers — write routing keys on shard_services via serves(),
-- read routing already resolves through ShardIDsServingService.
ALTER TABLE shard_records DROP COLUMN IF EXISTS apps;

-- +goose Down
-- Re-add the column (nullable; the old NOT NULL default was a JSON "[]").
-- Data is not reconstructed — shard_services is the source of truth now.
ALTER TABLE shard_records ADD COLUMN IF NOT EXISTS apps TEXT;
