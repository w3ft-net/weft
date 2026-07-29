-- +goose Up
-- +goose StatementBegin
--
-- Re-key the control plane from `customers.id bigint` to
-- `organizations.id uuid`, adopting the shared saaskit/orgs shape. A weft
-- Customer always *was* an org; this makes it one by the same name and the
-- same key type vimgym uses, so the two apps can share org/membership code.
--
-- This migration is data-preserving. The adoption plan was written when weft
-- had no customers and assumed drop-and-recreate; that is no longer true, so
-- every table gains its org_id by backfilling through the customer it already
-- points at, and only then loses customer_id.
--
-- What deliberately does NOT change: the log-data tenant boundary. Shard
-- queries isolate tenants with `WHERE customer = '<name>'` against DuckDB, and
-- the usage/archive rollup tables (usage_hourly, usage_daily,
-- stripe_usage_reports, shard_archive_stats) key off that same *name*. Name is
-- still `organizations.name`, still uniquely indexed, so none of that moves.
-- This migration re-keys surrogate ids only.

-- 1. Mint a UUID identity for every existing customer. This is the value the
--    whole schema is about to pivot onto, so it is generated once, here, and
--    every child backfills by joining to it.
ALTER TABLE public.customers ADD COLUMN org_id UUID NOT NULL DEFAULT gen_random_uuid();
ALTER TABLE public.customers ADD CONSTRAINT customers_org_id_key UNIQUE (org_id);

-- 2. Carry that identity down to every tenant-owned table.
--
--    Rows whose customer_id names a customer that no longer exists are deleted
--    rather than carried across. Half of these tables never had an FK to
--    customers, so orphans are possible; under the tenant-scoping callbacks
--    such a row is already unreachable (no context can name its owner), and it
--    cannot be given an org_id honestly. Dropping it is what "unreachable"
--    means made explicit.
DO $$
DECLARE
    t text;
    tenant_tables text[] := ARRAY[
        'agent_tokens', 'aws_accounts', 'cron_watch_history', 'cron_watches',
        'dashboards', 'log_groups', 'namespaces', 'notify_configs',
        'org_permissions', 'pagerduty_integrations', 'shell_function_history',
        'shell_functions', 'subscriptions', 'team_members', 'tmp_files'
    ];
BEGIN
    FOREACH t IN ARRAY tenant_tables LOOP
        EXECUTE format('ALTER TABLE public.%I ADD COLUMN org_id UUID', t);
        EXECUTE format(
            'UPDATE public.%I c SET org_id = o.org_id
               FROM public.customers o WHERE c.customer_id = o.id', t);
        EXECUTE format(
            'DELETE FROM public.%I WHERE org_id IS NULL', t);
        EXECUTE format('ALTER TABLE public.%I ALTER COLUMN org_id SET NOT NULL', t);
        -- Drops the column's indexes and FK along with it; both are rebuilt below.
        EXECUTE format('ALTER TABLE public.%I DROP COLUMN customer_id', t);
    END LOOP;
END $$;

-- tmp_files is partman-partitioned. The parent ALTER above reached every
-- existing partition, but future partitions are stamped from the template, so
-- it has to move too or tomorrow's partition arrives with the old column.
ALTER TABLE partman.template_public_tmp_files DROP COLUMN customer_id;
ALTER TABLE partman.template_public_tmp_files ADD COLUMN org_id UUID;

-- 3. Rebuild the indexes that dropping customer_id took with it, preserving
--    each one's exact shape (partial, ordering, uniqueness).
CREATE INDEX idx_cwh_org_name ON public.cron_watch_history USING btree (org_id, name, captured_at DESC);
CREATE UNIQUE INDEX idx_cron_org_name ON public.cron_watches USING btree (org_id, name);
CREATE UNIQUE INDEX idx_dashboards_org_name ON public.dashboards USING btree (org_id, name) WHERE (deleted_at IS NULL);
CREATE INDEX idx_log_groups_org ON public.log_groups USING btree (org_id);
CREATE UNIQUE INDEX idx_ns_org_name ON public.namespaces USING btree (org_id, name);
CREATE UNIQUE INDEX idx_notify_configs_org_id ON public.notify_configs USING btree (org_id);
CREATE UNIQUE INDEX idx_orgperm_org_user ON public.org_permissions USING btree (org_id, user_id);
CREATE UNIQUE INDEX idx_pd_integrations_org_name ON public.pagerduty_integrations USING btree (org_id, name) WHERE (deleted_at IS NULL);
CREATE INDEX idx_sfh_org_name ON public.shell_function_history USING btree (org_id, name, captured_at DESC);
CREATE UNIQUE INDEX idx_func_org_name ON public.shell_functions USING btree (org_id, name);
CREATE INDEX idx_subscriptions_org ON public.subscriptions USING btree (org_id);
CREATE INDEX idx_team_members_org ON public.team_members USING btree (org_id);
CREATE INDEX idx_tmp_files_org_path ON public.tmp_files USING btree (org_id, path);

-- 4. users points at "the org I was last looking at" — same re-key, but
--    nullable (a user may never have picked one) so no orphan delete.
ALTER TABLE public.users ADD COLUMN last_active_org_id UUID;
UPDATE public.users u SET last_active_org_id = c.org_id
  FROM public.customers c WHERE u.last_active_customer_id = c.id;
ALTER TABLE public.users DROP COLUMN last_active_customer_id;

-- 5. Now that nothing references customers.id, retire it and promote the UUID.
ALTER TABLE public.customers DROP CONSTRAINT customers_org_id_key;
ALTER TABLE public.customers DROP CONSTRAINT customers_pkey;
ALTER TABLE public.customers DROP COLUMN id;
ALTER TABLE public.customers RENAME COLUMN org_id TO id;
ALTER TABLE public.customers ADD PRIMARY KEY (id);
ALTER TABLE public.customers RENAME TO organizations;

-- Adopt the columns saaskit/orgs expects on an organization. weft keeps
-- billing detail in `subscriptions` (tier, comp, expiry — richer than orgs
-- models), so these carry the org-level Stripe identity and seat count only.
ALTER TABLE public.organizations ADD COLUMN stripe_customer_id TEXT;
ALTER TABLE public.organizations ADD COLUMN stripe_subscription_id TEXT;
ALTER TABLE public.organizations ADD COLUMN seat_count INTEGER NOT NULL DEFAULT 0;

-- 6. Restore the foreign keys, now pointing at organizations(id).
ALTER TABLE public.agent_tokens ADD CONSTRAINT fk_organizations_agent_tokens
    FOREIGN KEY (org_id) REFERENCES public.organizations(id) ON DELETE CASCADE;
ALTER TABLE public.aws_accounts ADD CONSTRAINT fk_organizations_accounts
    FOREIGN KEY (org_id) REFERENCES public.organizations(id) ON DELETE CASCADE;
ALTER TABLE public.log_groups ADD CONSTRAINT fk_log_groups_org
    FOREIGN KEY (org_id) REFERENCES public.organizations(id) ON DELETE CASCADE;
ALTER TABLE public.namespaces ADD CONSTRAINT fk_organizations_namespaces
    FOREIGN KEY (org_id) REFERENCES public.organizations(id) ON DELETE CASCADE;
ALTER TABLE public.pagerduty_integrations ADD CONSTRAINT fk_pd_integrations_org
    FOREIGN KEY (org_id) REFERENCES public.organizations(id) ON DELETE CASCADE;
ALTER TABLE public.subscriptions ADD CONSTRAINT fk_subscriptions_org
    FOREIGN KEY (org_id) REFERENCES public.organizations(id) ON DELETE CASCADE;
ALTER TABLE public.team_members ADD CONSTRAINT fk_team_members_org
    FOREIGN KEY (org_id) REFERENCES public.organizations(id) ON DELETE CASCADE;
ALTER TABLE public.users ADD CONSTRAINT users_last_active_org_id_fkey
    FOREIGN KEY (last_active_org_id) REFERENCES public.organizations(id) ON DELETE SET NULL;

-- 7. Re-key the trigger functions. Postgres does not resolve PL/pgSQL bodies
--    until they run, so these survived every ALTER above silently and would
--    have failed at the first write to a versioned table ("record 'new' has no
--    field customer_id") rather than at migration time.
CREATE OR REPLACE FUNCTION public.capture_cron_watch_history()
 RETURNS trigger
 LANGUAGE plpgsql
AS $function$
DECLARE
    last_history_id BIGINT;
    captured_by     TEXT;
    history_count   INT;
BEGIN
    IF OLD.content_hash = NEW.content_hash THEN
        RETURN NEW;
    END IF;

    captured_by := current_setting('weft.acting_user_id', true);
    IF captured_by = '' THEN
        captured_by := NULL;
    END IF;

    SELECT id INTO last_history_id
    FROM cron_watch_history
    WHERE watch_id = OLD.id
    ORDER BY captured_at DESC, id DESC
    LIMIT 1;

    INSERT INTO cron_watch_history (
        watch_id, org_id, name, schedule, pipeline, content_hash,
        message, captured_by_user_id, authored_by_user_id, predecessor_id
    ) VALUES (
        OLD.id, OLD.org_id, OLD.name, OLD.schedule, OLD.pipeline,
        OLD.content_hash, OLD.message, captured_by,
        OLD.created_by_user_id, last_history_id
    );

    SELECT count(*) INTO history_count
    FROM cron_watch_history
    WHERE watch_id = OLD.id;

    IF history_count > 5 THEN
        DELETE FROM cron_watch_history
        WHERE id = (
            SELECT id FROM cron_watch_history
            WHERE watch_id = OLD.id
            ORDER BY captured_at ASC, id ASC
            LIMIT 1
        );
    END IF;

    RETURN NEW;
END;
$function$;

CREATE OR REPLACE FUNCTION public.capture_shell_function_history()
 RETURNS trigger
 LANGUAGE plpgsql
AS $function$
DECLARE
    last_history_id BIGINT;
    captured_by     TEXT;
    history_count   INT;
BEGIN
    -- No-op save (open/close in :w without editing): the row content
    -- is unchanged, so don't pollute history with a duplicate hash.
    IF OLD.content_hash = NEW.content_hash THEN
        RETURN NEW;
    END IF;

    captured_by := current_setting('weft.acting_user_id', true);
    IF captured_by = '' THEN
        captured_by := NULL;
    END IF;

    SELECT id INTO last_history_id
    FROM shell_function_history
    WHERE function_id = OLD.id
    ORDER BY captured_at DESC, id DESC
    LIMIT 1;

    INSERT INTO shell_function_history (
        function_id, org_id, name, source, language, content_hash,
        message, captured_by_user_id, authored_by_user_id, predecessor_id
    ) VALUES (
        OLD.id, OLD.org_id, OLD.name, OLD.source, OLD.language,
        OLD.content_hash, OLD.message, captured_by,
        OLD.created_by_user_id, last_history_id
    );

    SELECT count(*) INTO history_count
    FROM shell_function_history
    WHERE function_id = OLD.id;

    IF history_count > 5 THEN
        DELETE FROM shell_function_history
        WHERE id = (
            SELECT id FROM shell_function_history
            WHERE function_id = OLD.id
            ORDER BY captured_at ASC, id ASC
            LIMIT 1
        );
    END IF;

    RETURN NEW;
END;
$function$;

-- The fill trigger is renamed along with the column it fills, and its
-- trigger rebound, so nothing still points at the old name.
CREATE OR REPLACE FUNCTION public.log_groups_fill_org_id()
 RETURNS trigger
 LANGUAGE plpgsql
AS $function$
BEGIN
  IF NEW.org_id IS NULL THEN
    SELECT org_id INTO NEW.org_id FROM namespaces WHERE id = NEW.namespace_id;
  END IF;
  RETURN NEW;
END;
$function$;

DROP TRIGGER IF EXISTS log_groups_fill_customer_id_bi ON public.log_groups;
DROP FUNCTION IF EXISTS public.log_groups_fill_customer_id();
CREATE TRIGGER log_groups_fill_org_id_bi
    BEFORE INSERT OR UPDATE ON public.log_groups
    FOR EACH ROW EXECUTE FUNCTION public.log_groups_fill_org_id();
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
-- Deliberately not reversible. Going back means re-minting the bigint surrogate
-- keys this migration retired; nothing retains the old values, so a "down" would
-- hand every tenant a new customer_id and silently re-point rows at the wrong
-- owner. Restore from a backup instead.
SELECT 1 / 0;
-- +goose StatementEnd
