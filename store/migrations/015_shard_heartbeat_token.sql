-- +goose Up
--
-- Per-shard bearer token for Heartbeat RPC authentication. Until this
-- column existed anyone with network reach to coord :9020 could register
-- a rogue shard (audit finding #1) — the provisioner's rebind path
-- trusted req.ShardId + req.Addr without any proof the caller was the
-- real owner of that ID.
--
-- The coordinator now mints a token during christen, returns it to the
-- shard in the Configure RPC response, and validates it on every
-- incoming heartbeat. Existing rows get a NULL token; the handler
-- treats nil-stored-token as "this shard predates the change, accept
-- any heartbeat during migration window" and logs a warning, so a
-- mid-deploy rolling restart doesn't orphan live shards. Once those
-- shards rotate (next launch / volume-rebind) they'll pick up a real
-- token and subsequent heartbeats become gated. Migration plan: run
-- this, deploy the coordinator, let the fleet roll, then follow up
-- with a NOT NULL constraint in 016.

ALTER TABLE shard_records ADD COLUMN heartbeat_token TEXT;

-- +goose Down
ALTER TABLE shard_records DROP COLUMN IF EXISTS heartbeat_token;
