-- +goose Up
-- Multi-row alert support. Watches can now extract a value from a chosen
-- field (-k) and a tag from another field (-t) of each stdout line, which
-- compiles down to one cron_results row per tag per tick. The extraction
-- fields live on cron_watches so we don't reparse the pipeline every tick;
-- the tag travels on cron_results and alert_incidents so aggregates and
-- dedup stay per-tag.

ALTER TABLE public.cron_watches
    ADD COLUMN alert_key INTEGER,
    ADD COLUMN alert_tag INTEGER;

ALTER TABLE public.cron_results
    ADD COLUMN tag TEXT NOT NULL DEFAULT '';

-- Replace the (cron_watch_id, ran_at DESC) index with one that carries the
-- tag. Aggregate queries always filter by watch+tag; putting tag in the
-- index middle lets us satisfy per-tag window scans without a heap touch.
DROP INDEX IF EXISTS public.idx_cron_results_watch_ran_at;
CREATE INDEX idx_cron_results_watch_tag_ran_at
    ON public.cron_results (cron_watch_id, tag, ran_at DESC);

ALTER TABLE public.alert_incidents
    ADD COLUMN tag TEXT NOT NULL DEFAULT '';

-- Incident identity now includes tag, so different tags on the same watch +
-- condition open and close independently. The old idx_incident_watch_condition
-- index wasn't unique; the new one is, to keep dedup honest.
DROP INDEX IF EXISTS public.idx_incident_watch_condition;
CREATE UNIQUE INDEX idx_incident_watch_condition_tag_open
    ON public.alert_incidents (cron_watch_id, condition, tag)
    WHERE closed_at IS NULL;
