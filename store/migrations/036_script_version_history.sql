-- +goose Up
-- pgcrypto provides digest() for the content-hash backfill.
CREATE EXTENSION IF NOT EXISTS pgcrypto;

-- Lightweight version history for /bin scripts and /cron.d watches.
-- Each save UPDATEs the parent row; a BEFORE-UPDATE trigger captures
-- the OLD row into a per-entity history table, scoped to UPDATE OF
-- the content columns so message-only edits (wit amend) don't
-- create spurious history rows. History is capped at 5 rows per
-- entity; the trigger drops the oldest when the cap would be
-- exceeded. See docs/plans/script-version-history.md.

-- 1. content_hash + message columns on the parent tables.
ALTER TABLE shell_functions
    ADD COLUMN content_hash TEXT NOT NULL DEFAULT '',
    ADD COLUMN message      TEXT NOT NULL DEFAULT '';

ALTER TABLE cron_watches
    ADD COLUMN content_hash TEXT NOT NULL DEFAULT '',
    ADD COLUMN message      TEXT NOT NULL DEFAULT '';

-- 2. Backfill content_hash on existing rows.
UPDATE shell_functions SET content_hash = encode(digest(source, 'sha256'), 'hex')
    WHERE content_hash = '';
UPDATE cron_watches SET content_hash = encode(digest(schedule || E'\n' || pipeline, 'sha256'), 'hex')
    WHERE content_hash = '';

-- 3. History tables.
CREATE TABLE shell_function_history (
    id                  BIGSERIAL PRIMARY KEY,
    function_id         BIGINT      NOT NULL,
    customer_id         BIGINT      NOT NULL,
    name                TEXT        NOT NULL,
    source              TEXT        NOT NULL,
    language            TEXT        NOT NULL,
    content_hash        TEXT        NOT NULL,
    message             TEXT        NOT NULL DEFAULT '',
    captured_at         TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    captured_by_user_id TEXT,
    authored_by_user_id TEXT,
    predecessor_id      BIGINT REFERENCES shell_function_history(id) ON DELETE SET NULL
    -- No UNIQUE on (function_id, captured_at): clock_timestamp()
    -- usually disambiguates, but if two saves land in the same
    -- microsecond on a fast machine, ordering by id breaks the tie
    -- in LogShellFunction. The PK (id) is the canonical identity.
);
CREATE INDEX idx_sfh_function_id   ON shell_function_history (function_id, captured_at DESC);
CREATE INDEX idx_sfh_customer_name ON shell_function_history (customer_id, name, captured_at DESC);
CREATE INDEX idx_sfh_content_hash  ON shell_function_history (function_id, content_hash);

CREATE TABLE cron_watch_history (
    id                  BIGSERIAL PRIMARY KEY,
    watch_id            BIGINT      NOT NULL,
    customer_id         BIGINT      NOT NULL,
    name                TEXT        NOT NULL,
    schedule            TEXT        NOT NULL,
    pipeline            TEXT        NOT NULL,
    content_hash        TEXT        NOT NULL,
    message             TEXT        NOT NULL DEFAULT '',
    captured_at         TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    captured_by_user_id TEXT,
    authored_by_user_id TEXT,
    predecessor_id      BIGINT REFERENCES cron_watch_history(id) ON DELETE SET NULL
);
CREATE INDEX idx_cwh_watch_id      ON cron_watch_history (watch_id, captured_at DESC);
CREATE INDEX idx_cwh_customer_name ON cron_watch_history (customer_id, name, captured_at DESC);
CREATE INDEX idx_cwh_content_hash  ON cron_watch_history (watch_id, content_hash);

-- 4. Trigger functions.

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION capture_shell_function_history()
RETURNS TRIGGER AS $$
DECLARE
    last_history_id BIGINT;
    captured_by     TEXT;
    history_count   INT;
BEGIN
    -- current_setting(name, true) returns '' (not NULL) for an
    -- unset GUC and never raises, which avoids the EXCEPTION
    -- gymnastics. We then collapse '' → NULL.
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
        function_id, customer_id, name, source, language, content_hash,
        message, captured_by_user_id, authored_by_user_id, predecessor_id
    ) VALUES (
        OLD.id, OLD.customer_id, OLD.name, OLD.source, OLD.language,
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
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION capture_cron_watch_history()
RETURNS TRIGGER AS $$
DECLARE
    last_history_id BIGINT;
    captured_by     TEXT;
    history_count   INT;
BEGIN
    -- current_setting(name, true) returns '' (not NULL) for an
    -- unset GUC and never raises, which avoids the EXCEPTION
    -- gymnastics. We then collapse '' → NULL.
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
        watch_id, customer_id, name, schedule, pipeline, content_hash,
        message, captured_by_user_id, authored_by_user_id, predecessor_id
    ) VALUES (
        OLD.id, OLD.customer_id, OLD.name, OLD.schedule, OLD.pipeline,
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
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

-- 5. Triggers, scoped to content-defining columns only.
CREATE TRIGGER capture_shell_function_history_trg
    BEFORE UPDATE OF source, language ON shell_functions
    FOR EACH ROW EXECUTE FUNCTION capture_shell_function_history();

CREATE TRIGGER capture_cron_watch_history_trg
    BEFORE UPDATE OF schedule, pipeline ON cron_watches
    FOR EACH ROW EXECUTE FUNCTION capture_cron_watch_history();
