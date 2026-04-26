-- +goose Up
-- Alert -k / -t selectors now accept either a 1-indexed positional column
-- (the only form before this migration) or a `.`-prefixed JSON path
-- (e.g. `.req.latency_ms`). Storage shifts from INTEGER to TEXT; existing
-- positional values cast cleanly to their digit-string form.

ALTER TABLE public.cron_watches
    ALTER COLUMN alert_key TYPE TEXT USING alert_key::TEXT,
    ALTER COLUMN alert_tag TYPE TEXT USING alert_tag::TEXT;

-- +goose Down
-- Reverting drops any JSON-path values; positional digit-strings cast back.
ALTER TABLE public.cron_watches
    ALTER COLUMN alert_key TYPE INTEGER USING NULLIF(alert_key, '')::INTEGER,
    ALTER COLUMN alert_tag TYPE INTEGER USING NULLIF(alert_tag, '')::INTEGER;
