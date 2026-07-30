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
-- 1. `id` becomes a real UUID. weft's own code always stored
--    uuid.New().String() here, but the TEXT column never enforced that and
--    production held rows that predate it — ids shaped
--    `demo-tm-<uuid>-<n>` from an older weft-demo-seed. Those are re-keyed
--    below rather than allowed to abort the cast; see the UPDATE for why
--    that is safe.
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

-- Re-key any id that is not already canonical UUID text, so the cast below
-- cannot be aborted by one bad row. A membership id is a bare surrogate:
-- nothing in the schema references it (it is a primary key with no inbound
-- foreign keys, and the table's only FK points outward at organizations),
-- so minting a fresh value loses no relationship. Aborting, by contrast,
-- takes the whole deploy down with it — which is exactly what happened on
-- 2026-07-30.
--
-- The regex is deliberately stricter than Postgres' uuid input, which also
-- accepts braced and unhyphenated forms. A non-canonical-but-castable id
-- would be re-keyed here too; that costs nothing for a value nothing
-- references, and keeps the predicate obvious.
UPDATE public.memberships
   SET id = gen_random_uuid()::text
 WHERE id !~ '^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$';

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

-- The two indexes below are UNIQUE where the columns they cover never were,
-- so any duplicate already sitting in the table would abort this migration
-- exactly as the uncastable id did. team_members enforced neither pair, and
-- the same legacy seed data that supplied the non-UUID ids may well have
-- inserted a user or address twice.
--
-- A duplicate membership is not information: two rows saying "this user
-- belongs to this org" describe one membership recorded twice. Collapse each
-- group to the row that best represents it — an accepted membership over a
-- pending one, the more privileged role over the lesser, then oldest, then
-- id for determinism — and drop the rest. Order matters: this runs after
-- owner→admin so the role test sees the unified vocabulary.
DELETE FROM public.memberships m
 USING (
    SELECT id,
           row_number() OVER (
               PARTITION BY org_id, user_id
               ORDER BY (accepted_at IS NOT NULL) DESC,
                        (role = 'admin') DESC,
                        created_at ASC NULLS LAST,
                        id ASC
           ) AS rn
      FROM public.memberships
     WHERE user_id IS NOT NULL
 ) dup
 WHERE m.id = dup.id AND dup.rn > 1;

DELETE FROM public.memberships m
 USING (
    SELECT id,
           row_number() OVER (
               PARTITION BY org_id, lower(email)
               ORDER BY (accepted_at IS NOT NULL) DESC,
                        (role = 'admin') DESC,
                        created_at ASC NULLS LAST,
                        id ASC
           ) AS rn
      FROM public.memberships
     WHERE email IS NOT NULL
 ) dup
 WHERE m.id = dup.id AND dup.rn > 1;

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
