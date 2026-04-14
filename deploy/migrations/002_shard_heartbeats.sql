-- Shard heartbeat table: shards push their state every 60s.
-- Partitioned by beat_at with 1-hour intervals, 5-minute retention.
-- A shard with no heartbeat in 2 minutes is considered dead.

-- Ensure pg_partman is available in its own schema.
CREATE SCHEMA IF NOT EXISTS partman;
CREATE EXTENSION IF NOT EXISTS pg_partman SCHEMA partman;

CREATE TABLE IF NOT EXISTS shard_heartbeats (
    shard_id     TEXT NOT NULL,
    addr         TEXT NOT NULL,
    instance_id  TEXT,
    apps         TEXT NOT NULL DEFAULT '[]',
    window_start TIMESTAMPTZ NOT NULL,
    window_end   TIMESTAMPTZ NOT NULL,
    state        TEXT NOT NULL,
    row_count    BIGINT NOT NULL DEFAULT 0,
    disk_bytes   BIGINT NOT NULL DEFAULT 0,
    beat_at      TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (shard_id, beat_at)
) PARTITION BY RANGE (beat_at);

-- pg_partman: auto-create/drop hourly partitions, keep 5 minutes of history.
SELECT partman.create_parent(
    p_parent_table := 'public.shard_heartbeats',
    p_control := 'beat_at',
    p_interval := '1 hour',
    p_premake := 2
);

UPDATE partman.part_config
SET retention = '5 minutes',
    retention_keep_table = false
WHERE parent_table = 'public.shard_heartbeats';
