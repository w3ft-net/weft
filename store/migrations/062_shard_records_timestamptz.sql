-- +goose Up
-- shard_records.window_start/window_end/created_at were TEXT (RFC3339)
-- from the original schema; last_heartbeat_at (migration 056) is already
-- timestamptz. Convert these three so the unified logdb.Shard type
-- (docs/plans/shard-type-collapse.md) maps them as native time.Time with
-- no string serializer. Every existing value was written via Go's
-- time.Format(time.RFC3339), which casts to timestamptz cleanly.
ALTER TABLE shard_records
    ALTER COLUMN window_start TYPE timestamptz USING window_start::timestamptz,
    ALTER COLUMN window_end   TYPE timestamptz USING window_end::timestamptz,
    ALTER COLUMN created_at   TYPE timestamptz USING created_at::timestamptz;

-- +goose Down
-- Reverse to RFC3339 TEXT (Zulu) so the pre-collapse string code can read
-- the values again. Best-effort: the down path is for emergency rollback.
ALTER TABLE shard_records
    ALTER COLUMN window_start TYPE text USING to_char(window_start AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'),
    ALTER COLUMN window_end   TYPE text USING to_char(window_end   AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'),
    ALTER COLUMN created_at   TYPE text USING to_char(created_at    AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"');
