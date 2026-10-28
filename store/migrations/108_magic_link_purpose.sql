-- +goose Up
-- A magic link now says what it was issued for, so the verify and accept
-- endpoints no longer infer intent from which rows happen to exist. A signup
-- link carries the pending org name and tier (nothing durable exists until
-- the address is verified); an invite link names the membership it accepts.
ALTER TABLE public.magic_links
    ADD COLUMN purpose text DEFAULT 'login'::text NOT NULL,
    ADD COLUMN org_name text DEFAULT ''::text NOT NULL,
    ADD COLUMN tier text DEFAULT ''::text NOT NULL,
    ADD COLUMN invite_id text DEFAULT ''::text NOT NULL;

-- One address is one account whatever its case. Emails are lowercased at the
-- store boundary from here on; fold the rows written before that.
UPDATE public.users SET email = lower(btrim(email)) WHERE email <> lower(btrim(email));
UPDATE public.magic_links SET email = lower(btrim(email)) WHERE email <> lower(btrim(email));
DROP INDEX public.idx_users_email;
CREATE UNIQUE INDEX idx_users_email ON public.users USING btree (lower(email));

-- +goose Down
DROP INDEX public.idx_users_email;
CREATE UNIQUE INDEX idx_users_email ON public.users USING btree (email);
ALTER TABLE public.magic_links
    DROP COLUMN purpose,
    DROP COLUMN org_name,
    DROP COLUMN tier,
    DROP COLUMN invite_id;
