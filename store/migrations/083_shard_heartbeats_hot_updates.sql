-- +goose Up
-- Every heartbeat upserts its source's row and rewrites beat_at. The index 081
-- put on beat_at is on a column those updates change, which disqualifies them
-- from being HOT (heap-only): each beat wrote a new tuple plus index entries.
-- Prod showed 16,988 updates and 0 HOT. The table holds one row per live shard,
-- so the planner seq-scans it anyway (4,299 sequential scans, no use of this
-- index) and the prune and lookback queries need no index.
--
-- fillfactor leaves room on the page for the new tuple versions HOT updates
-- chain through.

DROP INDEX public.shard_heartbeats_beat_at_idx;
ALTER TABLE public.shard_heartbeats SET (fillfactor = 50);

-- +goose Down
ALTER TABLE public.shard_heartbeats RESET (fillfactor);
CREATE INDEX shard_heartbeats_beat_at_idx ON public.shard_heartbeats (beat_at);
