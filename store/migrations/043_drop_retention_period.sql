-- +goose Up
ALTER TABLE buckets DROP COLUMN IF EXISTS retention_period;

-- +goose Down
ALTER TABLE buckets ADD COLUMN retention_period text NOT NULL DEFAULT '168h';
UPDATE buckets SET retention_period = window_duration;
