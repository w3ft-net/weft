-- +goose Up
-- When the most recent sample lines landed on a source, so the dashboard and
-- CLI can show how live a still-filling sample is ("last line 4s ago").
ALTER TABLE public.log_groups ADD COLUMN sample_updated_at timestamp with time zone;

-- +goose Down
ALTER TABLE public.log_groups DROP COLUMN sample_updated_at;
