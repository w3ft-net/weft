-- +goose Up
-- A cron's home namespace (docs/plans/tenant-scope.md, "Crons are
-- namespace-owned"). NULL is an org-level cron. Names are unique within a
-- home, so each home gets its own index.
ALTER TABLE public.cron_watches ADD COLUMN namespace_id bigint;
DROP INDEX public.idx_cron_org_name;
CREATE UNIQUE INDEX idx_cron_org_name ON public.cron_watches (org_id, name) WHERE namespace_id IS NULL;
CREATE UNIQUE INDEX idx_cron_org_ns_name ON public.cron_watches (org_id, namespace_id, name) WHERE namespace_id IS NOT NULL;

-- +goose Down
DROP INDEX public.idx_cron_org_ns_name;
DROP INDEX public.idx_cron_org_name;
ALTER TABLE public.cron_watches DROP COLUMN namespace_id;
CREATE UNIQUE INDEX idx_cron_org_name ON public.cron_watches (org_id, name);
