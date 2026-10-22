-- +goose Up
-- A cron that keeps exceeding its run budget is benched (enabled = false);
-- this counts the timeouts in a row so the bench is a fact of the row rather
-- than an in-memory tally lost to a coordinator restart or a lease handoff.
ALTER TABLE public.cron_watches ADD COLUMN consecutive_timeouts integer DEFAULT 0 NOT NULL;

-- +goose Down
ALTER TABLE public.cron_watches DROP COLUMN consecutive_timeouts;
