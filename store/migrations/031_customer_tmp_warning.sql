-- +goose Up
-- Per-customer flag tracking whether anyone on this account has been
-- shown the "files in /tmp expire after 48 hours" warning. Set the
-- first time a write to /tmp happens; once set, the warning never
-- prints again for that customer. NULL = never seen.

ALTER TABLE public.customers
    ADD COLUMN tmp_warning_seen_at TIMESTAMPTZ;
