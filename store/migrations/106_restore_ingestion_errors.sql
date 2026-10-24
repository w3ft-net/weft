-- +goose Up
-- Prod (weft-postgres) reached the collapsed high-water mark without this
-- table, although schema.sql defines it and no migration drops it; found
-- 2026-10-24 when `weft-admin audit` could not count it. Every DLQ'd batch's
-- record was failing to persist. IF NOT EXISTS makes this a no-op anywhere
-- the table is already present.
CREATE TABLE IF NOT EXISTS public.ingestion_errors (
    id          SERIAL PRIMARY KEY,
    occurred_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    shard_id    TEXT NOT NULL,
    error_msg   TEXT NOT NULL,
    row_count   INT NOT NULL,
    apps        TEXT[] NOT NULL,
    customers   TEXT[] NOT NULL,
    dlq_path    TEXT,
    resolved_at TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_ingestion_errors_unresolved
    ON public.ingestion_errors (occurred_at) WHERE resolved_at IS NULL;

-- +goose Down
-- Deliberately empty: the table predates this migration everywhere but prod.
SELECT 1;
