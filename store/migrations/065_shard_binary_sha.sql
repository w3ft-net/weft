-- +goose Up
-- shard_records.binary_sha records the compile-time GitSHA each shard
-- reports on its heartbeat. weft-admin reads it to decide whether a shard
-- already runs the target binary and can be skipped on a fleet roll —
-- shards are the thinnest binary in the system (gRPC over DuckDB) so most
-- deploys don't change them and shouldn't churn them.
--
-- Default "" so old rows hydrate cleanly; an empty value behaves like
-- "unknown SHA" (weft-admin treats unknown as not-current → rolls it).
ALTER TABLE shard_records
  ADD COLUMN binary_sha TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE shard_records DROP COLUMN binary_sha;
