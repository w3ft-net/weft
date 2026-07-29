-- +goose Up
-- +goose StatementBegin
--
-- Hand weft's membership table over to saaskit/orgs.
--
-- weft's team_members and orgs' memberships were already the same table in all
-- but name: both carry (org_id, email, nullable user_id, role, status,
-- invited_by/invited_at/accepted_at). orgs grew that shape *from* this one — it
-- is the richer of the two, which is why weft is adopting orgs rather than the
-- reverse. So this is a rename plus three reconciliations, not a rebuild.
--
-- 1. `id` becomes a real UUID. weft already stored uuid.New().String() in a TEXT
--    column, so every existing value casts cleanly.
-- 2. The role vocabulary unifies on orgs': weft's "owner" is orgs' "admin" —
--    same meaning (implicit full access, may grant groups), different word.
--    Keeping both would mean orgs.IsAdmin() returning false for weft's owners,
--    which is the one thing this adoption cannot tolerate.
-- 3. The partial unique indexes and the identity CHECK come across from
--    orgs.SchemaSQL so the library's ON CONFLICT clauses have indexes to infer.
--
-- What stays weft's: org_permissions (the binmgr/cronmgr/configmgr bits) and
-- namespace_permissions. orgs owns *whether* you are a member and at what role;
-- weft still owns what a member may touch.

ALTER TABLE public.team_members RENAME TO memberships;

ALTER TABLE public.memberships ALTER COLUMN id TYPE UUID USING id::uuid;
ALTER TABLE public.memberships ALTER COLUMN id SET DEFAULT gen_random_uuid();

-- Email is nullable in the shared shape: orgs.AddMember attaches a user the app
-- already knows and may have no address for. weft always had one, so no data
-- moves — only the constraint relaxes.
ALTER TABLE public.memberships ALTER COLUMN email DROP NOT NULL;

UPDATE public.memberships SET role = 'admin' WHERE role = 'owner';

-- A row nobody can address is a leak, not a membership.
ALTER TABLE public.memberships ADD CONSTRAINT memberships_identified
    CHECK (user_id IS NOT NULL OR email IS NOT NULL);

ALTER INDEX public.idx_team_members_org RENAME TO idx_memberships_org;
ALTER INDEX public.idx_team_members_user_id RENAME TO idx_memberships_user;

CREATE UNIQUE INDEX idx_memberships_org_user
    ON public.memberships(org_id, user_id) WHERE user_id IS NOT NULL;
-- Invites are matched case-insensitively so "Ada@x.com" cannot shadow "ada@x.com".
CREATE UNIQUE INDEX idx_memberships_org_email
    ON public.memberships(org_id, lower(email)) WHERE email IS NOT NULL;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS public.idx_memberships_org_email;
DROP INDEX IF EXISTS public.idx_memberships_org_user;
ALTER INDEX public.idx_memberships_user RENAME TO idx_team_members_user_id;
ALTER INDEX public.idx_memberships_org RENAME TO idx_team_members_org;
ALTER TABLE public.memberships DROP CONSTRAINT memberships_identified;
UPDATE public.memberships SET role = 'owner' WHERE role = 'admin';
DELETE FROM public.memberships WHERE email IS NULL;
ALTER TABLE public.memberships ALTER COLUMN email SET NOT NULL;
ALTER TABLE public.memberships ALTER COLUMN id DROP DEFAULT;
ALTER TABLE public.memberships ALTER COLUMN id TYPE TEXT USING id::text;
ALTER TABLE public.memberships RENAME TO team_members;
-- +goose StatementEnd
