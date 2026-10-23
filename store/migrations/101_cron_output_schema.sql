-- +goose Up
-- A cron's output schema (which field is the value, which the tag) no longer
-- comes only from its alert stage: `# value:`/`# tag:` headers set it too, so
-- the columns are named for what they hold. has_alert records whether the
-- cron ends in an alert stage, which decides how a flagless run is split.
-- Strikes now count every runtime check, not just timeouts.
ALTER TABLE public.cron_watches RENAME COLUMN alert_key TO value_field;
ALTER TABLE public.cron_watches RENAME COLUMN alert_tag TO tag_field;
ALTER TABLE public.cron_watches RENAME COLUMN consecutive_timeouts TO consecutive_strikes;
ALTER TABLE public.cron_watches ADD COLUMN has_alert boolean NOT NULL DEFAULT false;
UPDATE public.cron_watches SET has_alert = pipeline ~ '\malert\M';

-- +goose Down
ALTER TABLE public.cron_watches DROP COLUMN has_alert;
ALTER TABLE public.cron_watches RENAME COLUMN consecutive_strikes TO consecutive_timeouts;
ALTER TABLE public.cron_watches RENAME COLUMN tag_field TO alert_tag;
ALTER TABLE public.cron_watches RENAME COLUMN value_field TO alert_key;
