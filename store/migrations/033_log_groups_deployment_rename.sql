-- +goose Up
-- Rename log_groups.host_group → log_groups.deployment to match the new
-- vocabulary in the LogRow schema (k8s.deployment.name / AWS ASG / etc.).
-- The column was a "provenance breadcrumb" set during weftd onboarding;
-- the rename is purely cosmetic at the data level — every existing value
-- is preserved.
--
-- Idempotent: gorm AutoMigrate may have already created the new column
-- (the struct uses `gorm:"column:deployment"`), so we only rename when
-- the old column exists and the new one doesn't. See
-- docs/plans/ingest-dimensions.md.

-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (
    SELECT 1 FROM information_schema.columns
    WHERE table_schema = 'public'
      AND table_name = 'log_groups'
      AND column_name = 'host_group'
  ) AND NOT EXISTS (
    SELECT 1 FROM information_schema.columns
    WHERE table_schema = 'public'
      AND table_name = 'log_groups'
      AND column_name = 'deployment'
  ) THEN
    ALTER TABLE public.log_groups RENAME COLUMN host_group TO deployment;
  END IF;
END $$;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (
    SELECT 1 FROM information_schema.columns
    WHERE table_schema = 'public'
      AND table_name = 'log_groups'
      AND column_name = 'deployment'
  ) AND NOT EXISTS (
    SELECT 1 FROM information_schema.columns
    WHERE table_schema = 'public'
      AND table_name = 'log_groups'
      AND column_name = 'host_group'
  ) THEN
    ALTER TABLE public.log_groups RENAME COLUMN deployment TO host_group;
  END IF;
END $$;
-- +goose StatementEnd
