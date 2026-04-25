-- +goose Up
-- Per-user "have you seen this notice" tracking. The hard-coded
-- customers.tmp_warning_seen_at column from migration 031 was the
-- pre-generic precursor; we drop it here and route the existing
-- 48h-expiry notice through this table. Future notices add code
-- (a notice_key constant + registry entry) without touching the
-- schema.
--
-- The kind column is denormalized from the in-code registry so a
-- mismatch in flight (registry vs DB row) is loud, and so dashboard
-- queries don't need to read the registry to interpret rows.

CREATE TYPE notice_kind AS ENUM ('success', 'warning', 'error');

CREATE TABLE public.user_notices (
    user_id     TEXT        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    notice_key  TEXT        NOT NULL,
    kind        notice_kind NOT NULL,
    seen_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (user_id, notice_key)
);

ALTER TABLE public.customers DROP COLUMN tmp_warning_seen_at;
