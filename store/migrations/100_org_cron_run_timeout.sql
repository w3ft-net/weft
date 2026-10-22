-- +goose Up
-- Per-org override of the budget for one cron run (default 2s, set in the
-- coordinator). NULL means the default. Operator-set: a customer must not be
-- able to lift the limit that protects the shared fleet.
ALTER TABLE public.organizations ADD COLUMN cron_run_timeout_ms integer CHECK (cron_run_timeout_ms > 0);

-- +goose Down
ALTER TABLE public.organizations DROP COLUMN cron_run_timeout_ms;
