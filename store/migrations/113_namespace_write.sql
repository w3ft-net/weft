-- +goose Up
-- A namespace's write group (<ns>-w) manages its crons, tokens and sources
-- (docs/plans/tenant-scope.md, "Groups and the cron user"). It is the old
-- can_create_tokens bit, widened to cover crons; the org-wide cronmgr bit
-- (can_manage_watches) stops being read, so the people who held it get write
-- on every namespace they can read.
ALTER TABLE public.namespace_permissions RENAME COLUMN can_create_tokens TO can_write;
UPDATE public.namespace_permissions np
   SET can_write = true
  FROM public.namespaces n, public.org_permissions op
 WHERE n.id = np.namespace_id
   AND op.org_id = n.org_id
   AND op.user_id = np.user_id
   AND op.deleted_at IS NULL
   AND op.can_manage_watches
   AND np.can_read
   AND np.deleted_at IS NULL;

-- +goose Down
ALTER TABLE public.namespace_permissions RENAME COLUMN can_write TO can_create_tokens;
