-- +goose Up
-- One-time cleanup of shard_records cruft from the pre-durable-identity
-- era. Legacy rows have:
--   * id = EC2 instance id (i-...), because the old scheme used the
--     instance id as the shard id. Under the new scheme shard ids look
--     like `shard-YYYYMMDDThhmm-N` and live independently of instances.
--   * zero-value windows (empty string or `0001-01-01T00:00:00Z`),
--     because heartbeat-adopted rows never got a real window assigned.
--
-- These rows were never authoritative for any live shard; they only
-- made the kevcons state table noisy. Safe to delete.
--
-- Gap item #8 in docs/shard-lifecycle.md.
DELETE FROM shard_records
WHERE id LIKE 'i-%'
  AND (
    window_start IN ('', '0001-01-01T00:00:00Z')
    OR window_end IN ('', '0001-01-01T00:00:00Z')
  );

-- +goose Down
-- No-op: we can't resurrect deleted rows and we don't want to re-
-- populate shard_records with legacy shapes.
SELECT 1;
