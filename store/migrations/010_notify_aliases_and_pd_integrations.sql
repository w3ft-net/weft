-- +goose Up
-- Replace the flat notify.conf grammar (single `slack_webhook:` line) and the
-- loose PagerDutyKeys/Routes JSON blobs with a structured alias model:
--
--   * notify_configs.aliases_yaml — raw YAML of the alias map exposed as
--     /etc/notify.conf. Aliases look like:
--         ops:
--           slack: ["#ops-alerts"]
--           pagerduty:
--             - name: ops-daytime
--               severity: error
--
--   * pagerduty_integrations — 3NF table of customer-scoped PD integration
--     keys, keyed by the user-chosen name that notify.conf references.
--
-- Pre-launch, so no data preservation — the old pager_duty_keys/routes
-- columns are dropped outright.

CREATE TABLE IF NOT EXISTS pagerduty_integrations (
    id              BIGSERIAL PRIMARY KEY,
    created_at      TIMESTAMPTZ DEFAULT now(),
    updated_at      TIMESTAMPTZ DEFAULT now(),
    deleted_at      TIMESTAMPTZ,
    customer_id     BIGINT NOT NULL REFERENCES customers(id) ON DELETE CASCADE,
    name            TEXT NOT NULL,
    integration_key TEXT NOT NULL,
    description     TEXT,
    last_used_at    TIMESTAMPTZ,
    last_error      TEXT
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_pd_integrations_customer_name
    ON pagerduty_integrations (customer_id, name)
    WHERE deleted_at IS NULL;

CREATE INDEX IF NOT EXISTS idx_pd_integrations_deleted_at
    ON pagerduty_integrations (deleted_at);

ALTER TABLE notify_configs ADD COLUMN IF NOT EXISTS aliases_yaml TEXT NOT NULL DEFAULT '';
ALTER TABLE notify_configs DROP COLUMN IF EXISTS pager_duty_keys;
ALTER TABLE notify_configs DROP COLUMN IF EXISTS routes;

-- +goose Down
ALTER TABLE notify_configs ADD COLUMN IF NOT EXISTS routes TEXT;
ALTER TABLE notify_configs ADD COLUMN IF NOT EXISTS pager_duty_keys TEXT;
ALTER TABLE notify_configs DROP COLUMN IF EXISTS aliases_yaml;

DROP INDEX IF EXISTS idx_pd_integrations_deleted_at;
DROP INDEX IF EXISTS idx_pd_integrations_customer_name;
DROP TABLE IF EXISTS pagerduty_integrations;
