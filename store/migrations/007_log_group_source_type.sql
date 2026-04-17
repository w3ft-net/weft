-- +goose Up
-- Add an explicit source_type to log_groups so we can tell weftd-push
-- rows from CloudWatch-pull rows without the `agent://` URL-prefix hack.
-- The poller branches on this column; api/sources.go writes it.

CREATE TYPE log_group_source_type AS ENUM ('cloudwatch', 'weftd');

ALTER TABLE log_groups
    ADD COLUMN source_type log_group_source_type;

-- Backfill from the legacy prefix. Existing weftd rows used
-- log_group_name = 'agent://<app>'; CloudWatch rows use a real
-- arn/path.
UPDATE log_groups
    SET source_type = CASE
        WHEN log_group_name LIKE 'agent://%' THEN 'weftd'::log_group_source_type
        ELSE 'cloudwatch'::log_group_source_type
    END;

ALTER TABLE log_groups
    ALTER COLUMN source_type SET NOT NULL;

-- Strip the fake scheme from backfilled weftd rows so log_group_name
-- carries only the app path from here on. New inserts never write it.
UPDATE log_groups
    SET log_group_name = SUBSTRING(log_group_name FROM LENGTH('agent://') + 1)
    WHERE source_type = 'weftd'
      AND log_group_name LIKE 'agent://%';
