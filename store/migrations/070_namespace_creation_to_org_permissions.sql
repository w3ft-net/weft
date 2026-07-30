-- +goose Up
-- +goose StatementBegin
--
-- Move the namespace-creation grant from users onto org_permissions.
--
-- `users.can_create_namespace` was a global boolean authorizing an org-scoped
-- action: namespace creation happens inside the session's *active* org. A user
-- who belongs to two orgs therefore held the grant in both or in neither, and
-- an admin of one org could hand out a privilege that took effect inside
-- another. That is a cross-tenant leak by construction, not by accident of a
-- call site.
--
-- org_permissions is where the org-scoped grants already live (/bin, /cron.d,
-- /etc) and this bit has exactly their shape — per (org, member), default-deny
-- by absence — so it becomes the fourth one rather than a new table.

ALTER TABLE public.org_permissions
    ADD COLUMN can_create_namespaces BOOLEAN NOT NULL DEFAULT false;

-- Backfill. The grant was global, so it applied everywhere its holder could
-- act, and "everywhere they could act" is precisely their active memberships.
-- Fanning the flag out across those orgs is the only reading that preserves
-- what each user could actually do the moment before this migration ran.
--
-- Admins are included. Migration 072 makes org_permissions authoritative for
-- everyone — an admin's bits are explicit grants in a real row, not something
-- the session re-derives from the role on every request — so an admin holding
-- the global flag holds a real can_create_namespaces bit here, same as anyone
-- else. Skipping them would drop a grant they actually had.
--
-- Pending invites (user_id IS NULL) and removed memberships fall out of the
-- join and the status predicate respectively; neither can hold a permission.
INSERT INTO public.org_permissions
        (org_id, user_id, can_create_namespaces, created_at, updated_at)
SELECT m.org_id, m.user_id, true, now(), now()
  FROM public.memberships m
  JOIN public.users u ON u.id = m.user_id
 WHERE u.can_create_namespace
   AND m.status = 'active'
ON CONFLICT (org_id, user_id) DO UPDATE
    SET can_create_namespaces = true,
        updated_at            = now()
  -- The conflicting row may be a tombstone: removing a member soft-deletes
  -- their permissions, and re-adding them restores the membership without
  -- clearing those. Setting a bit on such a row would leave it invisible to the
  -- app anyway; clearing deleted_at to fix that would silently restore the
  -- *other three* bits the tombstone still carries. Neither is acceptable, so
  -- a tombstoned row is left alone and that (org, user) simply lands on
  -- default-deny — the safe direction.
  WHERE public.org_permissions.deleted_at IS NULL;

ALTER TABLE public.users DROP COLUMN can_create_namespace;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
--
-- Lossy by nature: the column being restored cannot express per-org grants, so
-- a user holding the bit in any one org gets it back globally — which is the
-- state this migration exists to eliminate. Reversible enough to unstick a bad
-- deploy, not a round trip.
ALTER TABLE public.users
    ADD COLUMN can_create_namespace BOOLEAN NOT NULL DEFAULT false;

UPDATE public.users u
   SET can_create_namespace = true
 WHERE EXISTS (
    SELECT 1 FROM public.org_permissions p
     WHERE p.user_id = u.id
       AND p.can_create_namespaces
       AND p.deleted_at IS NULL
 );

ALTER TABLE public.org_permissions DROP COLUMN can_create_namespaces;
-- +goose StatementEnd
