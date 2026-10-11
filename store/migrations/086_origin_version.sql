-- +goose Up
-- Tells a script's true first version from the oldest version that merely
-- survived the 5-version history cap. history.predecessor_id cannot: its FK
-- is ON DELETE SET NULL, so a trimmed chain's oldest row looks exactly like
-- an original one. origin_version_id is the version_id the object was
-- created with and never changes; a retained history row whose version_id
-- differs from it has had earlier versions trimmed away (a "shallow"
-- boundary in the git view of this history).

ALTER TABLE public.shell_functions ADD COLUMN origin_version_id bigint;
ALTER TABLE public.cron_watches ADD COLUMN origin_version_id bigint;

-- Backfill. An object with no history is still on its first version. With
-- fewer than 5 history rows nothing was ever trimmed, so the oldest history
-- row is the first version. With exactly 5 it is unknowable; NULL reads as
-- "assume trimmed", the safe side (the oldest version shows as a boundary).
UPDATE public.shell_functions f SET origin_version_id = CASE
    WHEN NOT EXISTS (SELECT 1 FROM public.shell_function_history h WHERE h.function_id = f.id)
        THEN f.version_id
    WHEN (SELECT count(*) FROM public.shell_function_history h WHERE h.function_id = f.id) < 5
        THEN (SELECT h.version_id FROM public.shell_function_history h
               WHERE h.function_id = f.id ORDER BY h.captured_at ASC, h.id ASC LIMIT 1)
    END;
UPDATE public.cron_watches w SET origin_version_id = CASE
    WHEN NOT EXISTS (SELECT 1 FROM public.cron_watch_history h WHERE h.watch_id = w.id)
        THEN w.version_id
    WHEN (SELECT count(*) FROM public.cron_watch_history h WHERE h.watch_id = w.id) < 5
        THEN (SELECT h.version_id FROM public.cron_watch_history h
               WHERE h.watch_id = w.id ORDER BY h.captured_at ASC, h.id ASC LIMIT 1)
    END;

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION public.set_authored_by_on_insert() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    NEW.authored_by_user_id := NEW.created_by_user_id;
    NEW.origin_version_id := NEW.version_id;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
