-- +goose Up
-- An incident is identified by its cron and tag, not by the text of the alert
-- stage, so the condition string leaves the key (editing a threshold used to
-- orphan the open incident, and --if-zero's expanded argument kept one from
-- ever closing). Whether a notification went out becomes a fact about a
-- (incident, target) pair rather than one notified_at on the incident, so a
-- failed or partial dispatch is retried only at the targets that missed it.

-- Collapse duplicate open incidents per (cron, tag) to the newest before the
-- narrower unique index goes on. A still-firing incident that survives has no
-- delivery rows, so its targets are notified once more on the next tick.
UPDATE public.alert_incidents a
   SET closed_at = now()
 WHERE a.closed_at IS NULL
   AND EXISTS (SELECT 1 FROM public.alert_incidents b
                WHERE b.closed_at IS NULL
                  AND b.cron_watch_id = a.cron_watch_id
                  AND b.tag = a.tag
                  AND (b.opened_at, b.id) > (a.opened_at, a.id));

DROP INDEX public.idx_incident_watch_condition_tag_open;
ALTER TABLE public.alert_incidents DROP COLUMN condition;
ALTER TABLE public.alert_incidents DROP COLUMN notified_at;
CREATE UNIQUE INDEX idx_incident_watch_tag_open
    ON public.alert_incidents (cron_watch_id, tag) WHERE closed_at IS NULL;

CREATE TABLE public.alert_incident_deliveries (
    incident_id  text NOT NULL REFERENCES public.alert_incidents(id) ON DELETE CASCADE,
    target       text NOT NULL,
    delivered_at timestamp with time zone NOT NULL,
    PRIMARY KEY (incident_id, target)
);

-- +goose Down
DROP TABLE public.alert_incident_deliveries;
DROP INDEX public.idx_incident_watch_tag_open;
ALTER TABLE public.alert_incidents ADD COLUMN notified_at timestamp with time zone;
ALTER TABLE public.alert_incidents ADD COLUMN condition text NOT NULL DEFAULT '';
CREATE UNIQUE INDEX idx_incident_watch_condition_tag_open
    ON public.alert_incidents (cron_watch_id, condition, tag) WHERE closed_at IS NULL;
