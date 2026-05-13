-- +goose Up

-- lowered_plan persists the planner.LoweredPlan JSON next to a
-- watch's source pipeline. Populated at save time by SaveCronWatch
-- when the pipeline lowers cleanly; NULL when the pipeline references
-- a /bin/ script the lowering pass can't yet inline. The fire path
-- uses lowered_plan when present (skipping preflight + Plan + the
-- customer-resolve DB roundtrip) and falls back to planner.Plan
-- inline when NULL.
--
-- Stored as JSONB so operators can inspect compiled SQL + bound arg
-- sources via `SELECT lowered_plan -> 'shard_sql' FROM cron_watches`.
ALTER TABLE cron_watches ADD COLUMN lowered_plan JSONB;

-- +goose Down
ALTER TABLE cron_watches DROP COLUMN lowered_plan;
