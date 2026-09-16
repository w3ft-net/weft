-- +goose Up
-- Generic mutual-exclusion primitive: "exactly one coordinator instance
-- may act on this named resource right now." Backs the CloudWatch
-- poller (one lease per log group) and the cron/watch executor (one
-- lease per watch) once the coordinator ASG scales past 1 instance --
-- see docs/plans/coordinator-horizontal-scaling.md. Small, bounded row
-- count (one row per concurrently-active leased resource), no
-- partitioning needed.
--
-- Acquire/renew is one atomic statement (store.AcquireLease):
--   INSERT ... ON CONFLICT (name) DO UPDATE ... WHERE holder = $holder OR expires_at < now()
-- RowsAffected > 0 means the caller now holds it (fresh, renewed, or
-- taken over from an expired holder); 0 means someone else holds a
-- non-expired lease. No separate read-then-write race window.
--
-- acquired_at tracks when the CURRENT holder's streak started (kept
-- across same-holder renewals, bumped only on takeover) so an operator
-- can see "how long has X held this" at a glance, not just "when was
-- this last renewed."

CREATE TABLE public.leases (
    name        text PRIMARY KEY,
    holder      text NOT NULL,
    expires_at  timestamptz NOT NULL,
    acquired_at timestamptz NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE IF EXISTS public.leases;
