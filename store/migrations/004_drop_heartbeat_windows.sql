-- +goose Up
-- Drop window_start and window_end from shard_heartbeats.
-- Shards are dumb storage — the coordinator manages time windows.
ALTER TABLE shard_heartbeats DROP COLUMN IF EXISTS window_start;
ALTER TABLE shard_heartbeats DROP COLUMN IF EXISTS window_end;
ALTER TABLE shard_heartbeats DROP COLUMN IF EXISTS apps;
