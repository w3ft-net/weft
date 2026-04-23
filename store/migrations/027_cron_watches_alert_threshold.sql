-- +goose Up
-- Surface the alert stage's condition + numeric threshold on the watch
-- row so the dashboard can draw a threshold line and "safe" shading on
-- each sparkline without re-parsing the pipeline in the browser.
--
-- Only populated for conditions that carry a single numeric threshold
-- (above, below, eq, neq). `rising`/`falling` use a percent delta, not
-- an absolute cutoff, and `changed`/`absent`/(no-keyword) have no
-- numeric threshold at all — all of those leave alert_threshold NULL.
--
-- Existing rows stay NULL until their next save; the coordinator still
-- parses the pipeline at tick time, so the absence of these fields is
-- harmless for enforcement — they're purely a dashboard projection.

ALTER TABLE public.cron_watches
    ADD COLUMN alert_condition TEXT,
    ADD COLUMN alert_threshold DOUBLE PRECISION;

-- +goose Down
ALTER TABLE public.cron_watches
    DROP COLUMN alert_threshold,
    DROP COLUMN alert_condition;
