-- +goose Up
-- When an upgrade first paused the shard's old instance: the start of the
-- window in which callers hold. It survives the pausing → swapping phase
-- change (upgrade_phase_at doesn't), so the leader that completes the swap
-- can log the whole hold even if another leader started it.
ALTER TABLE public.shard_records
    ADD COLUMN upgrade_paused_at timestamp with time zone;

-- +goose Down
ALTER TABLE public.shard_records
    DROP COLUMN upgrade_paused_at;
