-- +goose Up
-- Retention janitor bookkeeping.
--
-- last_purged_at: when the janitor last rewrote/revisited an archived
-- shard (drives "skip recently-purged" scheduling and observability).
ALTER TABLE shard_records ADD COLUMN last_purged_at TIMESTAMPTZ;

-- parquet_tombstones: objects the janitor has superseded (the old file
-- after a rewrite) or retired (a fully-expired shard's file). They are
-- NOT deleted inline — a GC sweep removes them past delete_after, a grace
-- window so an in-flight cold read isn't yanked mid-stream. A separate
-- table (not columns on shard_records) so a tombstone outlives the shard
-- row when a retired shard is reaped to GONE (onEnterGone deletes it).
CREATE TABLE parquet_tombstones (
    id           bigserial PRIMARY KEY,
    path         TEXT NOT NULL,           -- S3 path or glob to delete
    shard_id     TEXT NOT NULL,           -- origin shard (audit; not an FK — the row may be gone)
    reason       TEXT NOT NULL,           -- 'rewrite' | 'retention-delete'
    delete_after TIMESTAMPTZ NOT NULL,    -- earliest a GC sweep may delete it
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX parquet_tombstones_delete_after_idx
    ON parquet_tombstones (delete_after);

-- +goose Down
DROP TABLE IF EXISTS parquet_tombstones;
ALTER TABLE shard_records DROP COLUMN IF EXISTS last_purged_at;
