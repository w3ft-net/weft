-- +goose Up
-- Alert destinations now come from `alert --notify=<alias>` inside the
-- watch's pipeline, not from a per-watch metadata field. The column is
-- pre-launch and was only ever used by the cron executor's stderr→notify
-- bridge, which is also gone.
ALTER TABLE cron_watches DROP COLUMN IF EXISTS notify_dest;

-- +goose Down
ALTER TABLE cron_watches ADD COLUMN IF NOT EXISTS notify_dest TEXT;
