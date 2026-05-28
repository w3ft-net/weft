-- +goose NO TRANSACTION
-- +goose Up
-- Add 'direct' to log_group_source_type. Raw-push producers — Heroku
-- log drains, custom POSTs to /api/ingest, and any bare IngestStream
-- client — onboard with no GetSourceStatus/SubmitSample handshake (the
-- coordinator auto-creates the log_group on first-seen ingest). They're
-- neither the weftd agent nor the CloudWatch poller, so they need their
-- own source type. ADD VALUE can't run in a transaction, hence NO
-- TRANSACTION; IF NOT EXISTS makes the migration re-runnable.
ALTER TYPE public.log_group_source_type ADD VALUE IF NOT EXISTS 'direct';

-- +goose Down
-- Postgres can't drop an enum value without recreating the type and
-- rewriting every dependent column, which isn't worth it for a forward-
-- only value add. Leaving 'direct' in place on rollback is harmless.
SELECT 1;
