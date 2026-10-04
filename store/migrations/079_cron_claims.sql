-- +goose Up
-- One row per claimed cron slot: the primitive behind per-slot cron
-- execution (docs/plans/cron-claims-rollout.md). A coordinator runs a slot
-- only if its INSERT wins; a conflict means another coordinator owns it.
-- Keys are CronSlot.Key() values; shadow-mode dry runs use a 'shadow:'
-- prefix so they never collide with real claims. Rows are only useful
-- until the slot is past, so a cleanup job deletes old rows by claimed_at.

CREATE TABLE public.cron_claims (
    key        text PRIMARY KEY,
    holder     text NOT NULL,
    claimed_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX cron_claims_claimed_at_idx ON public.cron_claims (claimed_at);

-- +goose Down
DROP TABLE IF EXISTS public.cron_claims;
