-- +goose Up

-- Drop dead columns from namespace_permissions. CanManageWatches,
-- CanManageFunctions, and CanManageIntegrations were only ever written;
-- nothing read them. The new org_permissions table owns these concepts
-- because /bin, /cron.d, and /etc are org-scoped, not namespace-scoped.
ALTER TABLE namespace_permissions
    DROP COLUMN can_manage_watches,
    DROP COLUMN can_manage_functions,
    DROP COLUMN can_manage_integrations;

-- Org-scoped write permissions for the three directories that aren't
-- namespace-bound. Owners bypass; members with no row default to
-- read-only on /bin, /cron.d, /etc.
CREATE TABLE org_permissions (
    id bigserial PRIMARY KEY,
    created_at timestamp with time zone,
    updated_at timestamp with time zone,
    deleted_at timestamp with time zone,
    customer_id bigint NOT NULL,
    user_id text NOT NULL,
    can_manage_functions boolean NOT NULL DEFAULT false,
    can_manage_watches boolean NOT NULL DEFAULT false,
    can_manage_etc boolean NOT NULL DEFAULT false
);

CREATE UNIQUE INDEX idx_orgperm_customer_user
    ON org_permissions USING btree (customer_id, user_id);

CREATE INDEX idx_org_permissions_deleted_at
    ON org_permissions USING btree (deleted_at);

-- +goose Down

DROP TABLE org_permissions;

ALTER TABLE namespace_permissions
    ADD COLUMN can_manage_watches boolean NOT NULL DEFAULT false,
    ADD COLUMN can_manage_functions boolean NOT NULL DEFAULT false,
    ADD COLUMN can_manage_integrations boolean NOT NULL DEFAULT false;
