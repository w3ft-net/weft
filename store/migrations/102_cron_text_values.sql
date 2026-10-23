-- +goose Up
-- `# type: text` crons store state (which coordinator holds a lease) rather
-- than a measurement. Their values live in text_value with value NULL, so the
-- rollups, which summarise value, never see them.
ALTER TABLE public.cron_watches ADD COLUMN value_type text NOT NULL DEFAULT 'number' CHECK (value_type IN ('number', 'text'));
ALTER TABLE public.cron_results ADD COLUMN text_value text;
ALTER TABLE partman.template_public_cron_results ADD COLUMN text_value text;

-- +goose Down
ALTER TABLE partman.template_public_cron_results DROP COLUMN text_value;
ALTER TABLE public.cron_results DROP COLUMN text_value;
ALTER TABLE public.cron_watches DROP COLUMN value_type;
