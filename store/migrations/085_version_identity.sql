-- +goose Up
-- Stable identity and true authorship for each saved version of a /bin
-- script or /cron.d entry, so a git-shaped view of the history (see
-- docs/plans/wit-gitcore.md) can give every version a commit ID that does
-- not change when HEAD is displaced into the history table.
--
--   version_id          unique per content change (sequence); travels with
--                       the content from the live row into *_history
--   authored_at         when this version was written (history rows used to
--                       carry only captured_at, which is when the version was
--                       *replaced* — every older version's date was one
--                       step late)
--   authored_by_user_id who wrote this version (the acting user at the
--                       moment the content changed). On history rows it
--                       replaces the creator-of-the-object value the
--                       trigger used to copy.
--
-- Deletes are soft (deleted_at), so no tombstone trigger is needed: the
-- live row keeps its final content and deleted_at marks the deletion.

CREATE SEQUENCE public.object_version_seq;

ALTER TABLE public.shell_functions
    ADD COLUMN version_id bigint,
    ADD COLUMN authored_at timestamptz,
    ADD COLUMN authored_by_user_id text;
ALTER TABLE public.shell_function_history
    ADD COLUMN version_id bigint,
    ADD COLUMN authored_at timestamptz;
ALTER TABLE public.cron_watches
    ADD COLUMN version_id bigint,
    ADD COLUMN authored_at timestamptz,
    ADD COLUMN authored_by_user_id text;
ALTER TABLE public.cron_watch_history
    ADD COLUMN version_id bigint,
    ADD COLUMN authored_at timestamptz;

-- Backfill history first (it reads captured_by / captured_at of the
-- predecessor, which is the row that held the content before this one;
-- the user who displaced the predecessor wrote this version).
-- The oldest retained row has no predecessor: when fewer than 5 history
-- rows exist nothing was trimmed, so it is the object's first version and
-- dates from the live row's creation; otherwise fall back to its own
-- captured_at.
UPDATE public.shell_function_history h SET
    authored_at = COALESCE(
        (SELECT p.captured_at FROM public.shell_function_history p WHERE p.id = h.predecessor_id),
        CASE WHEN (SELECT count(*) FROM public.shell_function_history x WHERE x.function_id = h.function_id) < 5
             THEN (SELECT f.created_at FROM public.shell_functions f WHERE f.id = h.function_id) END,
        h.captured_at),
    authored_by_user_id = COALESCE(
        (SELECT p.captured_by_user_id FROM public.shell_function_history p WHERE p.id = h.predecessor_id),
        h.authored_by_user_id),
    version_id = nextval('public.object_version_seq');

UPDATE public.cron_watch_history h SET
    authored_at = COALESCE(
        (SELECT p.captured_at FROM public.cron_watch_history p WHERE p.id = h.predecessor_id),
        CASE WHEN (SELECT count(*) FROM public.cron_watch_history x WHERE x.watch_id = h.watch_id) < 5
             THEN (SELECT w.created_at FROM public.cron_watches w WHERE w.id = h.watch_id) END,
        h.captured_at),
    authored_by_user_id = COALESCE(
        (SELECT p.captured_by_user_id FROM public.cron_watch_history p WHERE p.id = h.predecessor_id),
        h.authored_by_user_id),
    version_id = nextval('public.object_version_seq');

-- Live rows: HEAD took over when the newest history row was captured.
UPDATE public.shell_functions f SET
    version_id = nextval('public.object_version_seq'),
    authored_at = COALESCE(
        (SELECT max(h.captured_at) FROM public.shell_function_history h WHERE h.function_id = f.id),
        f.created_at, now()),
    authored_by_user_id = COALESCE(
        (SELECT h.captured_by_user_id FROM public.shell_function_history h
          WHERE h.function_id = f.id ORDER BY h.captured_at DESC, h.id DESC LIMIT 1),
        f.created_by_user_id);

UPDATE public.cron_watches w SET
    version_id = nextval('public.object_version_seq'),
    authored_at = COALESCE(
        (SELECT max(h.captured_at) FROM public.cron_watch_history h WHERE h.watch_id = w.id),
        w.created_at, now()),
    authored_by_user_id = COALESCE(
        (SELECT h.captured_by_user_id FROM public.cron_watch_history h
          WHERE h.watch_id = w.id ORDER BY h.captured_at DESC, h.id DESC LIMIT 1),
        w.created_by_user_id);

ALTER TABLE public.shell_functions
    ALTER COLUMN version_id SET DEFAULT nextval('public.object_version_seq'),
    ALTER COLUMN version_id SET NOT NULL,
    ALTER COLUMN authored_at SET DEFAULT clock_timestamp(),
    ALTER COLUMN authored_at SET NOT NULL;
ALTER TABLE public.shell_function_history
    ALTER COLUMN version_id SET NOT NULL,
    ALTER COLUMN authored_at SET NOT NULL;
ALTER TABLE public.cron_watches
    ALTER COLUMN version_id SET DEFAULT nextval('public.object_version_seq'),
    ALTER COLUMN version_id SET NOT NULL,
    ALTER COLUMN authored_at SET DEFAULT clock_timestamp(),
    ALTER COLUMN authored_at SET NOT NULL;
ALTER TABLE public.cron_watch_history
    ALTER COLUMN version_id SET NOT NULL,
    ALTER COLUMN authored_at SET NOT NULL;

CREATE UNIQUE INDEX idx_shell_functions_version_id ON public.shell_functions (version_id);
CREATE UNIQUE INDEX idx_shell_function_history_version_id ON public.shell_function_history (version_id);
CREATE UNIQUE INDEX idx_cron_watches_version_id ON public.cron_watches (version_id);
CREATE UNIQUE INDEX idx_cron_watch_history_version_id ON public.cron_watch_history (version_id);

-- +goose StatementBegin
CREATE FUNCTION public.set_authored_by_on_insert() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    NEW.authored_by_user_id := NEW.created_by_user_id;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER shell_functions_authored_by_trg BEFORE INSERT ON public.shell_functions
    FOR EACH ROW EXECUTE FUNCTION public.set_authored_by_on_insert();
CREATE TRIGGER cron_watches_authored_by_trg BEFORE INSERT ON public.cron_watches
    FOR EACH ROW EXECUTE FUNCTION public.set_authored_by_on_insert();

-- Capture functions: same behaviour as before (copy OLD into history on a
-- content change, keep the newest 5), plus carry OLD's version identity and
-- authorship into the history row and mint a fresh identity for NEW.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION public.capture_shell_function_history() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
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
        message, captured_by_user_id, authored_by_user_id, predecessor_id,
        version_id, authored_at
    ) VALUES (
        OLD.id, OLD.org_id, OLD.name, OLD.source, OLD.language,
        OLD.content_hash, OLD.message, captured_by,
        OLD.authored_by_user_id, last_history_id,
        OLD.version_id, OLD.authored_at
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
        message, captured_by_user_id, authored_by_user_id, predecessor_id,
        version_id, authored_at
    ) VALUES (
        OLD.id, OLD.org_id, OLD.name, OLD.schedule, OLD.pipeline,
        OLD.content_hash, OLD.message, captured_by,
        OLD.authored_by_user_id, last_history_id,
        OLD.version_id, OLD.authored_at
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
