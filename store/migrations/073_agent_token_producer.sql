-- +goose Up
-- Add the producer column that scopes a token to one syslog Producer
-- (e.g. "render") so store.ValidateAgentTokenForProducer can reject a
-- token minted for a different producer -- or a plain weftd/JSON/Heroku
-- token, which has no producer at all -- from being replayed against a
-- SyslogServer listener. See docs/plans/render.md, Component 3.
--
-- Empty string means "not producer-scoped" -- every existing token, and
-- every token minted by the non-syslog ingest paths, keeps working
-- exactly as before.
--
-- Idempotent: gorm AutoMigrate may have already added the column from
-- the AgentToken struct's new Producer field -- that's fine, IF NOT
-- EXISTS skips the ADD when present.

ALTER TABLE public.agent_tokens
  ADD COLUMN IF NOT EXISTS producer TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE public.agent_tokens
  DROP COLUMN IF EXISTS producer;
