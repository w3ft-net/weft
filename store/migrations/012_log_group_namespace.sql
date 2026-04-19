-- +goose Up
--
-- Pin every log_groups row to a namespace so we have a single authoritative
-- "does customer X own source Y" lookup. Before this migration:
--   - weftd-sourced rows encoded ownership as "namespace/subapp" in the app
--     column (string parsing, no referential integrity).
--   - cloudwatch-sourced rows had no namespace concept — just a flat app
--     name pinned to an AWS Account.
-- After: every row has a namespace_id FK; weftd rows no longer need the
-- "agent" placeholder Account that api/sources.go used to mint.

-- 1. Add the column, nullable for backfill.
ALTER TABLE log_groups
    ADD COLUMN namespace_id BIGINT REFERENCES namespaces(id) ON DELETE CASCADE;

-- 2. Backfill weftd rows from the existing app "namespace/subapp" convention.
--    Join through the account to scope the namespace to the right customer.
UPDATE log_groups lg
   SET namespace_id = ns.id
  FROM accounts a, namespaces ns
 WHERE lg.account_id = a.id
   AND lg.source_type = 'weftd'
   AND ns.customer_id = a.customer_id
   AND ns.name = split_part(lg.app, '/', 1);

-- 3. Backfill cloudwatch rows. These had no namespace concept — auto-create
--    a namespace with the same name as the app's leading segment for the
--    owning customer.
INSERT INTO namespaces (customer_id, name, created_at, updated_at)
SELECT DISTINCT a.customer_id,
                split_part(lg.app, '/', 1),
                now(), now()
  FROM log_groups lg
  JOIN accounts a ON a.id = lg.account_id
 WHERE lg.source_type = 'cloudwatch'
ON CONFLICT (customer_id, name) DO NOTHING;

UPDATE log_groups lg
   SET namespace_id = ns.id
  FROM accounts a, namespaces ns
 WHERE lg.account_id = a.id
   AND lg.source_type = 'cloudwatch'
   AND ns.customer_id = a.customer_id
   AND ns.name = split_part(lg.app, '/', 1);

-- 4. Any row still unpinned means the backfill missed a case — fail the
--    migration before the NOT NULL would do it silently.
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM log_groups WHERE namespace_id IS NULL) THEN
        RAISE EXCEPTION 'migration 012: log_groups rows still have NULL namespace_id after backfill';
    END IF;
END$$;
-- +goose StatementEnd

ALTER TABLE log_groups
    ALTER COLUMN namespace_id SET NOT NULL;

-- 5. Replace the (account_id, app) uniqueness constraint with (namespace_id,
--    app) — namespace is now the ownership key. Drop the old unique index if
--    it exists (gorm created it under one of two names depending on version).
ALTER TABLE log_groups
    DROP CONSTRAINT IF EXISTS idx_account_app;
DROP INDEX IF EXISTS idx_account_app;
CREATE UNIQUE INDEX IF NOT EXISTS idx_ns_app
    ON log_groups(namespace_id, app);

-- 6. weftd sources never actually needed an Account — api/sources.go used
--    to mint a placeholder "agent" account per customer to satisfy the FK.
--    Make account_id optional and remove those placeholders.
ALTER TABLE log_groups
    ALTER COLUMN account_id DROP NOT NULL;

UPDATE log_groups
   SET account_id = NULL
 WHERE source_type = 'weftd';

DELETE FROM accounts a
 WHERE a.aws_account = 'agent'
   AND NOT EXISTS (SELECT 1 FROM log_groups lg WHERE lg.account_id = a.id);

-- +goose Down

ALTER TABLE log_groups
    DROP COLUMN IF EXISTS namespace_id;

ALTER TABLE log_groups
    ALTER COLUMN account_id SET NOT NULL;

DROP INDEX IF EXISTS idx_ns_app;
