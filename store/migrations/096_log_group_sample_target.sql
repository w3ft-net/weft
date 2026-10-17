-- +goose Up
-- How many lines the source's current sample is collecting toward, when a
-- resample asked for more than the default 50. NULL means the default.
ALTER TABLE public.log_groups ADD COLUMN sample_target integer;

-- +goose Down
ALTER TABLE public.log_groups DROP COLUMN sample_target;
