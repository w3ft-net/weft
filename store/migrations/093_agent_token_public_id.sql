-- +goose Up
-- Tokens get an opaque public id (tok_<16 hex>) used by the API, CLI, and web
-- UI to name a token for revoke. The bigint primary key no longer leaves the
-- store: it is guessable, and exposing it invited cross-tenant probing.
ALTER TABLE public.agent_tokens ADD COLUMN public_id text;
UPDATE public.agent_tokens
   SET public_id = 'tok_' || substr(md5(random()::text || id::text || clock_timestamp()::text), 1, 16);
ALTER TABLE public.agent_tokens ALTER COLUMN public_id SET NOT NULL;
CREATE UNIQUE INDEX agent_tokens_public_id_key ON public.agent_tokens (public_id);

-- +goose Down
DROP INDEX public.agent_tokens_public_id_key;
ALTER TABLE public.agent_tokens DROP COLUMN public_id;
