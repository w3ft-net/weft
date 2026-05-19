-- +goose Up
-- Resample + sampling-progress support for log sources.
--
-- resample_requested_at is set when a user asks for a fresh sample of
-- an already-configured source. The agent re-runs the sampler
-- concurrently with its tailer — ingestion is NOT interrupted, the
-- source stays configured on its current schema — and resubmits a
-- fresh sample for review. The coordinator clears this column on the
-- final (non-partial) submit.
--
-- sample_collected tracks accumulation progress while status =
-- 'sampling', so callers can surface
-- "<source> hasn't emitted enough lines to sample yet (N/50)".
--
-- The status check gains 'sampling': a source the agent has announced
-- but not yet fully sampled. Lifecycle: sampling -> pending ->
-- configured. A configured source with a pending resample stays
-- 'configured' (so it keeps ingesting); its fresh sample lands in the
-- existing sample/detection columns.

ALTER TABLE log_groups
  ADD COLUMN IF NOT EXISTS resample_requested_at TIMESTAMPTZ,
  ADD COLUMN IF NOT EXISTS sample_collected      INTEGER;

ALTER TABLE log_groups DROP CONSTRAINT IF EXISTS log_groups_status_check;
ALTER TABLE log_groups
  ADD CONSTRAINT log_groups_status_check
  CHECK (status = ANY (ARRAY['sampling'::text, 'pending'::text, 'configured'::text]));

-- +goose Down
ALTER TABLE log_groups DROP CONSTRAINT IF EXISTS log_groups_status_check;
ALTER TABLE log_groups
  ADD CONSTRAINT log_groups_status_check
  CHECK (status = ANY (ARRAY['pending'::text, 'configured'::text]));

ALTER TABLE log_groups
  DROP COLUMN IF EXISTS resample_requested_at,
  DROP COLUMN IF EXISTS sample_collected;
