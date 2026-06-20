-- +goose Up
-- Per-(shard, customer) archive metadata: the row count and ts span of
-- each customer's data within an archived shard's parquet. Captured at
-- archive time (logdb/shardsm.go, from the Archive RPC's customer_stats)
-- so the retention janitor can classify archived files as
-- delete / keep / rewrite without re-scanning S3. See
-- docs/plans/retention-janitor.md.
--
-- ON DELETE CASCADE: when a shard_records row is removed (a GONE shard's
-- cleanup), its stats go with it — they only describe that shard's file.
CREATE TABLE shard_archive_stats (
    shard_id  TEXT NOT NULL REFERENCES shard_records(id) ON DELETE CASCADE,
    customer  TEXT NOT NULL,
    min_ts    TIMESTAMPTZ NOT NULL,
    max_ts    TIMESTAMPTZ NOT NULL,
    row_count BIGINT NOT NULL,
    PRIMARY KEY (shard_id, customer)
);

-- Janitor planner reads "which shards hold over-retained rows for
-- customer X" — index the customer dimension for that lookup.
CREATE INDEX shard_archive_stats_customer_idx
    ON shard_archive_stats (customer);

-- +goose Down
DROP TABLE IF EXISTS shard_archive_stats;
