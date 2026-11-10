-- +goose Up
-- +goose StatementBegin
-- The org-wide cronmgr bit is retired: a namespace's write group manages its
-- crons (migration 113 gave its holders write on the namespaces they read).
-- The membership trigger stops materializing it, then the column goes.
CREATE OR REPLACE FUNCTION public.sync_membership_org_permissions() RETURNS trigger
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
                (org_id, user_id, can_manage_functions,
                 can_manage_etc, can_create_namespaces, created_at, updated_at)
        VALUES (NEW.org_id, NEW.user_id, true, true, true, now(), now())
        ON CONFLICT (org_id, user_id) DO UPDATE
            SET can_manage_functions  = true,
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
ALTER TABLE public.org_permissions DROP COLUMN can_manage_watches;

-- +goose Down
ALTER TABLE public.org_permissions ADD COLUMN can_manage_watches boolean DEFAULT false NOT NULL;
