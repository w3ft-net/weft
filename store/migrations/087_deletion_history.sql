-- +goose Up
-- A deletion must survive the name being saved again. Deletes are soft
-- (deleted_at), and saving over a deleted name revives the row in place,
-- which used to erase the only record of the deletion. Now a revive moves
-- the deleted version into history even when its content is unchanged, and
-- the history row remembers when that version was deleted. The git view
-- shows a deleted history row as a version followed by a deletion.

ALTER TABLE public.shell_function_history ADD COLUMN deleted_at timestamptz;
ALTER TABLE public.cron_watch_history ADD COLUMN deleted_at timestamptz;

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION public.capture_shell_function_history() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    last_history_id BIGINT;
    captured_by     TEXT;
    history_count   INT;
    revived         BOOLEAN;
BEGIN
    -- No-op save (open/close in :w without editing): the row content
    -- is unchanged, so don't pollute history with a duplicate hash. A revive
    -- (deleted_at cleared) is never a no-op: the deletion has to be kept.
    revived := OLD.deleted_at IS NOT NULL AND NEW.deleted_at IS NULL;
    IF OLD.content_hash = NEW.content_hash AND NOT revived THEN
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
        message, captured_by_user_id, authored_by_user_id, predecessor_id,
        version_id, authored_at, deleted_at
    ) VALUES (
        OLD.id, OLD.org_id, OLD.name, OLD.source, OLD.language,
        OLD.content_hash, OLD.message, captured_by,
        OLD.authored_by_user_id, last_history_id,
        OLD.version_id, OLD.authored_at, OLD.deleted_at
    );

    NEW.version_id := nextval('public.object_version_seq');
    NEW.authored_at := clock_timestamp();
    NEW.authored_by_user_id := captured_by;

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
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION public.capture_cron_watch_history() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    last_history_id BIGINT;
    captured_by     TEXT;
    history_count   INT;
    revived         BOOLEAN;
BEGIN
    -- No-op save (open/close in :w without editing): the row content
    -- is unchanged, so don't pollute history with a duplicate hash. A revive
    -- (deleted_at cleared) is never a no-op: the deletion has to be kept.
    revived := OLD.deleted_at IS NOT NULL AND NEW.deleted_at IS NULL;
    IF OLD.content_hash = NEW.content_hash AND NOT revived THEN
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
        message, captured_by_user_id, authored_by_user_id, predecessor_id,
        version_id, authored_at, deleted_at
    ) VALUES (
        OLD.id, OLD.org_id, OLD.name, OLD.schedule, OLD.pipeline,
        OLD.content_hash, OLD.message, captured_by,
        OLD.authored_by_user_id, last_history_id,
        OLD.version_id, OLD.authored_at, OLD.deleted_at
    );

    NEW.version_id := nextval('public.object_version_seq');
    NEW.authored_at := clock_timestamp();
    NEW.authored_by_user_id := captured_by;

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
$$;
-- +goose StatementEnd

-- The trigger must fire when a revive rewrites only deleted_at.
DROP TRIGGER capture_shell_function_history_trg ON public.shell_functions;
CREATE TRIGGER capture_shell_function_history_trg BEFORE UPDATE OF source, language, deleted_at ON public.shell_functions FOR EACH ROW EXECUTE FUNCTION public.capture_shell_function_history();
DROP TRIGGER capture_cron_watch_history_trg ON public.cron_watches;
CREATE TRIGGER capture_cron_watch_history_trg BEFORE UPDATE OF schedule, pipeline, deleted_at ON public.cron_watches FOR EACH ROW EXECUTE FUNCTION public.capture_cron_watch_history();
