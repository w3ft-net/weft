-- +goose Up
-- The /bin scripts a cron runs, directly or through other scripts, recorded as
-- the cron is saved (docs/plans/tenant-scope.md). A write to a script is checked
-- against the namespaces of the crons that depend on it.
CREATE TABLE public.cron_bin_deps (
    org_id        bigint NOT NULL,
    cron_watch_id bigint NOT NULL REFERENCES public.cron_watches(id) ON DELETE CASCADE,
    function_id   bigint NOT NULL REFERENCES public.shell_functions(id) ON DELETE CASCADE,
    PRIMARY KEY (cron_watch_id, function_id)
);
CREATE INDEX idx_cron_bin_deps_function ON public.cron_bin_deps (function_id);

-- +goose Down
DROP TABLE IF EXISTS public.cron_bin_deps;
