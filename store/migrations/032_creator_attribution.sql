-- +goose Up
-- Track the user who created each user-content row across the three
-- writable namespaces. Nullable: NULL means "no attribution" — system
-- writes (cron executor, weftd ingest path), pre-existing rows from
-- before this work, or future automation paths. The ls -al renderer
-- shows a dash for NULL.
--
-- No FK to users(id): consistent with weft's other per-user fields,
-- and lets a user's row be soft-deleted without cascading into the
-- content tables. Display-name lookups handle missing users
-- gracefully.

ALTER TABLE public.shell_functions
    ADD COLUMN created_by_user_id TEXT;

ALTER TABLE public.cron_watches
    ADD COLUMN created_by_user_id TEXT;

ALTER TABLE public.tmp_files
    ADD COLUMN created_by_user_id TEXT;
