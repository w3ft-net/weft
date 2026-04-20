-- +goose Up
--
-- Merge pending_sources into log_groups. The two tables used different
-- keys — pending on (customer_id, namespace_text, app) and log_groups on
-- (namespace_id, "namespace/app") — so nothing stopped an incoming
-- weftd sample from UPSERTing a pending row for a source that was
-- already configured. Result in prod: the UI showed the same source as
-- BOTH pending and configured.
--
-- After this migration: one row per (namespace_id, app). "Configure"
-- becomes an UPDATE (status='pending' → 'configured' + schema_yaml
-- filled). A sample arriving for an already-configured row is a no-op.
--
-- source_type stays orthogonal: it identifies where the bytes come from
-- (weftd push vs cloudwatch pull), not whether a human has approved
-- the schema. Nothing today creates a pending cloudwatch row, but the
-- schema supports it for a future auto-discovery UX.

-- 1. New columns on log_groups. sample/detection hold JSON payloads
--    but stored as TEXT — we don't query them from SQL, and TEXT lets
--    GORM send the Go zero-value "" without tripping JSONB validation.
ALTER TABLE log_groups
    ADD COLUMN status     TEXT NOT NULL DEFAULT 'configured',
    ADD COLUMN instance   TEXT,
    ADD COLUMN host_group TEXT,
    ADD COLUMN sample     TEXT,
    ADD COLUMN detection  TEXT;

ALTER TABLE log_groups
    ADD CONSTRAINT log_groups_status_check
    CHECK (status IN ('pending', 'configured'));

-- 2. Pending rows don't yet have a schema or a confirmed log-group name.
ALTER TABLE log_groups
    ALTER COLUMN schema_yaml    DROP NOT NULL,
    ALTER COLUMN log_group_name DROP NOT NULL;

-- 3. Move pending_sources rows into log_groups. Namespace is resolved
--    from the existing namespaces table; orphans (a pending_sources row
--    whose customer/namespace pair has no namespaces row) are skipped —
--    they were never reachable through the configure UI anyway.
--    app convention in log_groups is "namespace/subapp"; pending stored
--    the parts separately, so we compose here.
INSERT INTO log_groups (
    created_at, updated_at,
    namespace_id, app, source_type, log_group_name, region,
    schema_yaml, poll_interval, enabled,
    status, instance, host_group, sample, detection
)
SELECT
    ps.created_at,
    ps.created_at,
    ns.id,
    ps.namespace || '/' || ps.app,
    'weftd'::log_group_source_type,
    ps.namespace || '/' || ps.app,
    'local',
    NULL,
    '0s',
    true,
    'pending',
    ps.instance,
    ps.host_group,
    ps.sample::text,
    ps.detection::text
FROM pending_sources ps
JOIN namespaces ns
    ON ns.customer_id = ps.customer_id AND ns.name = ps.namespace
ON CONFLICT (namespace_id, app) DO NOTHING;

-- 4. Drop the old table and its FK.
DROP TABLE pending_sources;

-- +goose Down

CREATE TABLE pending_sources (
    customer_id BIGINT NOT NULL REFERENCES customers(id) ON DELETE CASCADE,
    namespace   TEXT   NOT NULL,
    app         TEXT   NOT NULL,
    instance    TEXT,
    host_group  TEXT,
    sample      JSONB  NOT NULL,
    detection   JSONB  NOT NULL,
    created_at  TIMESTAMPTZ,
    PRIMARY KEY (customer_id, namespace, app)
);

CREATE UNIQUE INDEX idx_pending_customer_ns_app
    ON pending_sources(customer_id, namespace, app);

INSERT INTO pending_sources (customer_id, namespace, app, instance, host_group, sample, detection, created_at)
SELECT
    ns.customer_id,
    ns.name,
    split_part(lg.app, '/', 2),
    lg.instance,
    lg.host_group,
    COALESCE(lg.sample, '[]')::jsonb,
    COALESCE(lg.detection, '{}')::jsonb,
    lg.created_at
FROM log_groups lg
JOIN namespaces ns ON ns.id = lg.namespace_id
WHERE lg.status = 'pending';

DELETE FROM log_groups WHERE status = 'pending';

ALTER TABLE log_groups
    ALTER COLUMN schema_yaml    SET NOT NULL,
    ALTER COLUMN log_group_name SET NOT NULL;

ALTER TABLE log_groups
    DROP CONSTRAINT IF EXISTS log_groups_status_check;

ALTER TABLE log_groups
    DROP COLUMN IF EXISTS status,
    DROP COLUMN IF EXISTS instance,
    DROP COLUMN IF EXISTS host_group,
    DROP COLUMN IF EXISTS sample,
    DROP COLUMN IF EXISTS detection;
