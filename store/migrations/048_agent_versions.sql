-- +goose Up

-- last_seen_version captures the X-Weft-Client / weft-client gRPC
-- metadata the agent reports on each connection. Used to decide when
-- it's safe to delete an old-shape wire parser branch on the server:
-- find tokens with last_seen_version below the bump's floor and an
-- old enough last_seen_at, and you know the carry-over code can go.
--
-- Stored as text (not semver-parsed) so the format can grow — current
-- shape is "0.14.0" or "0.14.0+abcdef" for released builds.
ALTER TABLE agent_tokens
  ADD COLUMN last_seen_version text NOT NULL DEFAULT '',
  ADD COLUMN last_seen_at      timestamptz;

-- Index for the deprecation query: "any token still on version < X
-- seen in the last 14d?" — version-prefix scan + timestamp filter.
CREATE INDEX idx_agent_tokens_version_seen ON agent_tokens (last_seen_version, last_seen_at);

-- +goose Down
DROP INDEX IF EXISTS idx_agent_tokens_version_seen;
ALTER TABLE agent_tokens DROP COLUMN last_seen_at;
ALTER TABLE agent_tokens DROP COLUMN last_seen_version;
