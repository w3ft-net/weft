-- +goose Up
-- Persisted facts for the ephemeral archive export
-- (docs/plans/ephemeral-archive-export.md). The shard FSM derives the export's
-- phase from these instead of adding FSM states: a snapshot with no instance
-- means "launch next", an instance with no manifest means "wait", and so on.
--
-- export_attempt is monotonic and survives clearing the other facts: it caps
-- retries and keeps each attempt's AWS tags distinct. The others are cleared
-- when an attempt ends, whether it succeeded or failed.

ALTER TABLE public.shard_records
    ADD COLUMN export_snapshot_id   text,
    ADD COLUMN export_instance_id   text,
    ADD COLUMN export_instance_type text,
    ADD COLUMN export_started_at    timestamptz,
    ADD COLUMN export_attempt       integer NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE public.shard_records
    DROP COLUMN IF EXISTS export_snapshot_id,
    DROP COLUMN IF EXISTS export_instance_id,
    DROP COLUMN IF EXISTS export_instance_type,
    DROP COLUMN IF EXISTS export_started_at,
    DROP COLUMN IF EXISTS export_attempt;
