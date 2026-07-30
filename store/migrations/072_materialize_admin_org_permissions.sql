-- +goose Up
-- +goose StatementBegin
--
-- org_permissions becomes authoritative.
--
-- "An admin may write /bin, /cron.d, /etc" used to be a derived fact:
-- nothing stored it, BuildSession recomputed it from memberships.role on every
-- request, and any question of the form "who can write /cron.d" was a union of
-- org_permissions rows and admin memberships. Two permission systems ran side
-- by side, one explicit and one implicit, and every read site had to know
-- about both.
--
-- After this migration org_permissions holds a row for every active member,
-- admins included, and that row is the only answer. Role goes back to meaning
-- what its name says — who may manage members and grants.
--
-- The decision that makes it work: promotion materializes the four bits as
-- explicit grants, and demotion LEAVES THEM IN PLACE. Because demotion keeps
-- them, a set bit means "explicitly granted" in every state, so there is no
-- provenance to track and nothing for this trigger to guess. The alternative —
-- demotion clears the bits — has to know which of them predated the promotion,
-- and that was never recorded. The retention is announced rather than silent:
-- demoting someone emits a notice naming exactly what they kept.

-- ---------------------------------------------------------------------------
-- 1. Materialize the admins that already exist.
-- ---------------------------------------------------------------------------
--
-- The join to users is load-bearing, not decoration. memberships.user_id has
-- no foreign key, and prod holds membership rows pointing at users that no
-- longer exist. Without the join this INSERT would launder those orphans into
-- permission rows.
--
-- A conflicting row may be a tombstone (removed-then-re-added member). Unlike
-- migration 070, reviving it here is right: the target is an *active admin*,
-- who is entitled to all four bits regardless of what the tombstone carried,
-- and after this migration a live row is what makes them able to act at all.
INSERT INTO public.org_permissions
        (org_id, user_id, can_manage_functions, can_manage_watches,
         can_manage_etc, can_create_namespaces, created_at, updated_at)
SELECT m.org_id, m.user_id, true, true, true, true, now(), now()
  FROM public.memberships m
  JOIN public.users u ON u.id = m.user_id
 WHERE m.role = 'admin'
   AND m.status = 'active'
ON CONFLICT (org_id, user_id) DO UPDATE
    SET can_manage_functions  = true,
        can_manage_watches    = true,
        can_manage_etc        = true,
        can_create_namespaces = true,
        deleted_at            = NULL,
        updated_at            = now();
-- +goose StatementEnd

-- +goose StatementBegin
--
-- ---------------------------------------------------------------------------
-- 2. Keep them materialized.
-- ---------------------------------------------------------------------------
--
-- This function is the sole writer of admin-derived permission state.
-- RemoveMember's hand-rolled soft-delete moves in here with it, which removes
-- a two-writers-one-row hazard rather than creating one.
CREATE OR REPLACE FUNCTION public.sync_membership_org_permissions()
RETURNS trigger
LANGUAGE plpgsql
AS $$
DECLARE
    perm     public.org_permissions%ROWTYPE;
    retained text[];
    actor    text;
    subject  text;
    org_name text;
BEGIN
    -- A pending invite has no user to grant anything to.
    IF NEW.user_id IS NULL THEN
        RETURN NEW;
    END IF;

    -- Removal tombstones the row. Re-adding the member restores the
    -- membership but not the grants; they are re-granted explicitly.
    IF NEW.status = 'removed' THEN
        UPDATE public.org_permissions
           SET deleted_at = now(),
               updated_at = now()
         WHERE org_id = NEW.org_id
           AND user_id = NEW.user_id
           AND deleted_at IS NULL;
        RETURN NEW;
    END IF;

    -- Promotion, or an admin becoming active: all four bits, explicitly.
    -- Idempotent, so it is safe on any UPDATE that merely touches role or
    -- status without changing them.
    IF NEW.role = 'admin' AND NEW.status = 'active' THEN
        INSERT INTO public.org_permissions
                (org_id, user_id, can_manage_functions, can_manage_watches,
                 can_manage_etc, can_create_namespaces, created_at, updated_at)
        VALUES (NEW.org_id, NEW.user_id, true, true, true, true, now(), now())
        ON CONFLICT (org_id, user_id) DO UPDATE
            SET can_manage_functions  = true,
                can_manage_watches    = true,
                can_manage_etc        = true,
                can_create_namespaces = true,
                deleted_at            = NULL,
                updated_at            = now();
        RETURN NEW;
    END IF;

    -- Demotion. The bits stay; the operator is told what stayed.
    IF TG_OP <> 'UPDATE' OR OLD.role <> 'admin' OR NEW.role = 'admin' THEN
        RETURN NEW;
    END IF;

    SELECT * INTO perm
      FROM public.org_permissions
     WHERE org_id = NEW.org_id
       AND user_id = NEW.user_id
       AND deleted_at IS NULL;
    IF NOT FOUND THEN
        RETURN NEW;
    END IF;

    retained := ARRAY(
        SELECT g FROM (VALUES
            ('binmgr',    perm.can_manage_functions),
            ('cronmgr',   perm.can_manage_watches),
            ('configmgr', perm.can_manage_etc),
            ('nsmgr',     perm.can_create_namespaces)
        ) AS t(g, held) WHERE held
    );
    IF cardinality(retained) = 0 THEN
        RETURN NEW;
    END IF;

    -- Who to tell. set_config(..., true) is transaction-local, so this is
    -- only non-empty when the caller set it on this very connection. An
    -- out-of-band change (psql, a script) has no operator to inform, and
    -- silence is the correct outcome there — not an error.
    actor := coalesce(current_setting('weft.acting_user_id', true), '');
    IF actor = '' THEN
        RETURN NEW;
    END IF;
    -- user_notices.user_id is FK'd to users. A stale or bogus GUC must not
    -- take down a membership update, so check before inserting.
    IF NOT EXISTS (SELECT 1 FROM public.users WHERE id = actor) THEN
        RETURN NEW;
    END IF;

    subject := coalesce(
        NEW.email,
        (SELECT email FROM public.users WHERE id = NEW.user_id),
        NEW.user_id);

    -- Name the org. Notices are addressed to a user, not to a (user, org)
    -- pair, and the operator can be reading with any of their memberships
    -- selected in the switcher — so an org-scoped fact that doesn't say
    -- which org is a riddle. The renderer leads with it.
    SELECT name INTO org_name FROM public.organizations WHERE id = NEW.org_id;

    INSERT INTO public.user_notices (user_id, notice_key, kind, params, created_at)
    VALUES (actor,
            'member_retained_grants',
            'warning'::public.notice_kind,
            jsonb_build_object(
                'org',    coalesce(org_name, NEW.org_id::text),
                'member', subject,
                'groups', to_jsonb(retained)),
            now());

    RETURN NEW;
END;
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER sync_membership_org_permissions
    AFTER INSERT OR UPDATE OF role, status ON public.memberships
    FOR EACH ROW EXECUTE FUNCTION public.sync_membership_org_permissions();
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TRIGGER IF EXISTS sync_membership_org_permissions ON public.memberships;
DROP FUNCTION IF EXISTS public.sync_membership_org_permissions();

-- Un-materializing is lossy in the direction that matters: the bits an admin
-- was granted before the promotion are indistinguishable from the ones the
-- promotion created, which is the whole premise of the forward migration.
-- Clearing every active admin's row restores the old shape (admins derive
-- from role, hold no rows) at the cost of any grant they held as a member
-- before being promoted.
UPDATE public.org_permissions p
   SET can_manage_functions  = false,
       can_manage_watches    = false,
       can_manage_etc        = false,
       can_create_namespaces = false,
       updated_at            = now()
 WHERE EXISTS (
    SELECT 1 FROM public.memberships m
     WHERE m.org_id = p.org_id
       AND m.user_id = p.user_id
       AND m.role = 'admin'
       AND m.status = 'active'
 );
-- +goose StatementEnd
