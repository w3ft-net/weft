-- +goose NO TRANSACTION
-- +goose Up
-- Add 'kinesis' to log_group_source_type. Push-based CloudWatch
-- ingestion (docs/plans/cloudwatch-kinesis-subscription.md) — a
-- customer-owned Kinesis Data Firehose delivery stream posting to
-- POST /api/ingest/cloudwatch — is neither the poll-based 'cloudwatch'
-- path nor the weftd agent nor a bare push producer, so it needs its
-- own source type rather than overloading an existing one with a flag
-- column (that would make every existing SourceType switch silently
-- wrong until updated). ADD VALUE can't run in a transaction, hence
-- NO TRANSACTION; IF NOT EXISTS makes the migration re-runnable.
ALTER TYPE public.log_group_source_type ADD VALUE IF NOT EXISTS 'kinesis';

-- +goose Down
-- Postgres can't drop an enum value without recreating the type and
-- rewriting every dependent column, which isn't worth it for a forward-
-- only value add. Leaving 'kinesis' in place on rollback is harmless.
SELECT 1;
