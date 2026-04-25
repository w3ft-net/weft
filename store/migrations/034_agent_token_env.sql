-- +goose Up
-- Add the env column for token-derived deployment-environment dimensions.
-- Coordinator IngestStream stamps this on every row from the bound token;
-- the agent has no say in the value. See docs/plans/ingest-dimensions.md.
--
-- Idempotent: gorm AutoMigrate may have already added the column from
-- the AgentToken struct's new Env field — that's fine, IF NOT EXISTS
-- skips the ADD when present.

ALTER TABLE public.agent_tokens
  ADD COLUMN IF NOT EXISTS env TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE public.agent_tokens
  DROP COLUMN IF EXISTS env;
