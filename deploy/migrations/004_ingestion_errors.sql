-- Ingestion error tracking for DLQ and dashboard banner.
CREATE TABLE IF NOT EXISTS ingestion_errors (
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
    ON ingestion_errors (occurred_at) WHERE resolved_at IS NULL;
