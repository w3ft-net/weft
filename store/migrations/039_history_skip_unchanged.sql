-- +goose Up
-- The capture_*_history triggers (migration 036) fire on every UPDATE OF
-- the content columns, regardless of whether the new value differs from
-- the old. A common interactive pattern (open file in :w, close without
-- editing) therefore appended duplicate-hash rows to history, breaking
-- HEAD^N navigation and making `wit diff` choke on hash prefixes that
-- match multiple identical entries. Fix: skip the INSERT when the
-- content_hash is unchanged. Also collapse the consecutive duplicate
-- runs already present in production history.

-- 1. Trigger functions: early-return on identical content_hash.

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION capture_shell_function_history()
RETURNS TRIGGER AS $$
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

-- 2. Cleanup: collapse consecutive duplicate-hash rows already in
-- history. For each (entity, hash) run of adjacent rows, keep only
-- the oldest (the row that actually marks the transition into that
-- content) and drop the rest. Predecessor pointers are then re-stitched
-- so the chain stays intact.

-- +goose StatementBegin
DO $$
DECLARE
    fn_id BIGINT;
BEGIN
    FOR fn_id IN
        SELECT DISTINCT function_id FROM shell_function_history
    LOOP
        WITH ordered AS (
            SELECT id, content_hash,
                   LAG(content_hash) OVER (ORDER BY captured_at ASC, id ASC) AS prev_hash
            FROM shell_function_history
            WHERE function_id = fn_id
        )
        DELETE FROM shell_function_history
        WHERE id IN (
            SELECT id FROM ordered WHERE content_hash = prev_hash
        );

        -- Re-stitch predecessor_id: each remaining row should point at
        -- the row immediately older than itself in this function's chain.
        WITH chain AS (
            SELECT id,
                   LAG(id) OVER (ORDER BY captured_at ASC, id ASC) AS prev_id
            FROM shell_function_history
            WHERE function_id = fn_id
        )
        UPDATE shell_function_history h
        SET predecessor_id = chain.prev_id
        FROM chain
        WHERE h.id = chain.id;
    END LOOP;
END $$;
-- +goose StatementEnd

-- +goose StatementBegin
DO $$
DECLARE
    w_id BIGINT;
BEGIN
    FOR w_id IN
        SELECT DISTINCT watch_id FROM cron_watch_history
    LOOP
        WITH ordered AS (
            SELECT id, content_hash,
                   LAG(content_hash) OVER (ORDER BY captured_at ASC, id ASC) AS prev_hash
            FROM cron_watch_history
            WHERE watch_id = w_id
        )
        DELETE FROM cron_watch_history
        WHERE id IN (
            SELECT id FROM ordered WHERE content_hash = prev_hash
        );

        WITH chain AS (
            SELECT id,
                   LAG(id) OVER (ORDER BY captured_at ASC, id ASC) AS prev_id
            FROM cron_watch_history
            WHERE watch_id = w_id
        )
        UPDATE cron_watch_history h
        SET predecessor_id = chain.prev_id
        FROM chain
        WHERE h.id = chain.id;
    END LOOP;
END $$;
-- +goose StatementEnd

-- After collapsing consecutive duplicates, a no-op save that landed
-- against a legitimate non-duplicate predecessor leaves exactly one
-- trailing history row whose hash equals HEAD. Drop only that newest
-- row when it matches — never older rows, because an older history
-- row matching HEAD is the signature of a legitimate revert (HEAD
-- was rolled back to a pre-existing version) and dropping it would
-- erase real history.

-- +goose StatementBegin
WITH newest AS (
    SELECT DISTINCT ON (function_id) id, function_id, content_hash
    FROM shell_function_history
    ORDER BY function_id, captured_at DESC, id DESC
)
DELETE FROM shell_function_history h
USING newest, shell_functions f
WHERE h.id = newest.id
  AND newest.function_id = f.id
  AND newest.content_hash = f.content_hash;
-- +goose StatementEnd

-- +goose StatementBegin
WITH newest AS (
    SELECT DISTINCT ON (watch_id) id, watch_id, content_hash
    FROM cron_watch_history
    ORDER BY watch_id, captured_at DESC, id DESC
)
DELETE FROM cron_watch_history h
USING newest, cron_watches w
WHERE h.id = newest.id
  AND newest.watch_id = w.id
  AND newest.content_hash = w.content_hash;
-- +goose StatementEnd

-- +goose Down

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION capture_shell_function_history()
RETURNS TRIGGER AS $$
DECLARE
    last_history_id BIGINT;
    captured_by     TEXT;
    history_count   INT;
BEGIN
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
