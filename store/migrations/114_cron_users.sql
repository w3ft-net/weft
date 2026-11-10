-- +goose Up
-- Every namespace has a cron user, "cron:<namespace id>", in its own read
-- group (docs/plans/tenant-scope.md, "Groups and the cron user"). New
-- namespaces get the row when they are created; this seeds the existing ones.
INSERT INTO public.namespace_permissions (created_at, updated_at, user_id, namespace_id, can_read, can_write)
SELECT now(), now(), 'cron:' || n.id, n.id, true, false
  FROM public.namespaces n
 WHERE n.deleted_at IS NULL
   AND NOT EXISTS (SELECT 1 FROM public.namespace_permissions p
                    WHERE p.user_id = 'cron:' || n.id AND p.namespace_id = n.id);

-- +goose Down
DELETE FROM public.namespace_permissions WHERE user_id LIKE 'cron:%';
