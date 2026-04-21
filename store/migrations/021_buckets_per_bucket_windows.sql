-- +goose Up
-- Per-bucket window_duration and retention_period. Moves those two
-- knobs off ProvisionerConfig (which was coordinator-global) onto
-- the bucket row, so different buckets can run at different
-- cadences on the same tier — e.g. a dedicated-per-app bucket for
-- a chatty service might want shorter windows than the default
-- catch-all. Also required for multi-bucket-per-tier ASG sizing
-- to be honest (max_capacity sums (2 + ceil(retention/window)) *
-- target over buckets-in-tier; mixing durations needs per-bucket
-- values).
--
-- Values are stored as Go time.Duration text literals ("168h",
-- "30m"). Defaults match DefaultProvisionerConfig: 7d/7d.

ALTER TABLE buckets
    ADD COLUMN IF NOT EXISTS window_duration  text NOT NULL DEFAULT '168h',
    ADD COLUMN IF NOT EXISTS retention_period text NOT NULL DEFAULT '168h';

-- +goose Down
-- No-op — dropping columns would erase per-bucket config that may
-- differ from the coordinator-global defaults.
