-- +goose Up
-- Normalize log_groups.app to the bare service name, dropping the
-- redundant "<namespace>/" prefix that weftd's SubmitSample used to
-- prepend (server.go built fullApp = auth.Namespace + "/" + req.App).
--
-- The namespace is already carried by namespace_id, and service-level
-- shard routing keys on the BARE service — the shard rows' `service`
-- column (SplitApp drops the namespace) and the planner's `WHERE
-- service = ?` both use the bare form. The prefixed form ("weft/api")
-- therefore broke routing for weftd sources: shard_services.service
-- (stamped from log_groups.app) carried "weft/api" while queries asked
-- for "api". Every prod source is weftd, so this fixes routing for all
-- of them.
--
-- Namespace-aware strip (match the row's own namespace name) so a
-- service whose name happens to contain a slash isn't truncated.
UPDATE log_groups lg
SET app = substring(lg.app FROM length(n.name) + 2)
FROM namespaces n
WHERE lg.namespace_id = n.id
  AND lg.app LIKE n.name || '/%';

-- Rebuild shard_services from the corrected log_groups. Migration 059
-- stamped it from the prefixed app, so existing rows carry the stale
-- composite service; every shard_services row is derivable from current
-- log_groups (christen stamps the same join), so a clean rebuild is
-- correct. Same join as 059: shard_records ⋈ log_groups on
-- COALESCE(bucket_id, default) = sr.bucket_id.
DELETE FROM shard_services;
INSERT INTO shard_services (shard_id, namespace_id, service)
SELECT sr.id, lg.namespace_id, lg.app
FROM shard_records sr
JOIN log_groups lg
  ON COALESCE(lg.bucket_id, (SELECT id FROM buckets WHERE label = 'default')) = sr.bucket_id
WHERE lg.deleted_at IS NULL
ON CONFLICT (shard_id, namespace_id, service) DO NOTHING;

-- +goose Down
-- No clean inverse: the original "<namespace>/" prefix can be
-- reconstructed from namespace_id, but doing so would re-break routing,
-- and shard_services would need re-stamping from the re-prefixed rows.
-- Not worth the footgun for a normalization that's strictly correct.
SELECT 1;
