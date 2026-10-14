-- +goose Up
-- Tokens are no longer scoped to an ingest/syslog.Producer: the listener
-- (port + cert) already identifies the platform, so any live token for the
-- namespace authenticates there like it does everywhere else. Existing
-- render-scoped tokens keep working.
ALTER TABLE public.agent_tokens DROP COLUMN producer;

-- +goose Down
ALTER TABLE public.agent_tokens ADD COLUMN producer text DEFAULT ''::text NOT NULL;
