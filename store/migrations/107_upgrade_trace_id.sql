-- +goose Up
-- The trace of the RelaunchShard call that started an upgrade, so the phase
-- ticks (which have no request) and a new leader continuing the upgrade log
-- under it, with the request's span as the parent of the spans it records.
-- Cleared with the other upgrade_* facts when the upgrade ends.
ALTER TABLE public.shard_records ADD COLUMN IF NOT EXISTS upgrade_trace_id TEXT;
ALTER TABLE public.shard_records ADD COLUMN IF NOT EXISTS upgrade_parent_span_id TEXT;

-- +goose Down
ALTER TABLE public.shard_records DROP COLUMN IF EXISTS upgrade_trace_id;
ALTER TABLE public.shard_records DROP COLUMN IF EXISTS upgrade_parent_span_id;
