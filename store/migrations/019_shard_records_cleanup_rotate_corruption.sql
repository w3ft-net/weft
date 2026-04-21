-- +goose Up
-- Cleanup rows in shard_records corrupted by the legacy rotate() /
-- provisionForBucket() paths in coordinator/provisioner.go (both
-- removed in the commit this migration pairs with).
--
-- Two classes of bad data are targeted:
--
-- 1. Loopback-addr ghosts. The dev-mode LocalLauncher spawns shards as
--    in-process gRPC servers on 127.0.0.1:<random> and writes that
--    addr into shard_records. Those processes die with their
--    coordinator; on restart LoadFromStore only rehydrates ARCHIVED
--    rows, so these entries are orphans that reapDeadShards can never
--    see (it iterates Registry.Entries, not the store). Safe to
--    delete.
--
-- 2. Far-future windows. rotate() and provisionForBucket() used to
--    set window_end = now + 100 years for replacement shards (a
--    vestige of the pre-window provisioner that assumed windows
--    didn't exist). Registry.MatchingShards gates queries by
--    WindowStart.Before(tr.End) && WindowEnd.After(tr.Start) — a
--    shard with a year-2126 window_end matches every future query
--    and would spread the blast radius of any lingering corruption.
--
-- rotate() also truncated legitimate window_ends to the wall-clock
-- timestamp at rotation time (window_end = now on the outgoing
-- shard). We don't undo that here: WindowDuration is a runtime
-- config value, not a schema constraint, so we can't reliably
-- reconstruct what the end "should" have been. A surviving shard's
-- shard.json on disk has the real identity; the coordinator will
-- route queries according to what the DB says.

DELETE FROM shard_records WHERE addr LIKE '127.%';

DELETE FROM shard_records WHERE window_end::timestamptz > '2100-01-01';

-- +goose Down
-- Destructive cleanup — no meaningful rollback. Re-creating the
-- corrupted rows would just put the same broken data back.
