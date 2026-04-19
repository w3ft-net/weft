-- +goose Up
--
-- Agent tokens are now stored as sha256 hex digests plus a short
-- prefix for display (audit finding #1). The agent keeps the raw
-- wft_<32hex> string; the coordinator only ever persists its hash.
-- Same rationale as migration 016 for magic links: a DB leak can't
-- hand an attacker usable credentials, and the dev seed token now
-- lives at rest as a hash too.
--
-- There is no path to recover the pre-hash raw tokens, so any
-- existing rows would be permanently unusable after this migration.
-- Delete them — pre-launch we have no customers, and on prod the
-- coordinator will re-issue fresh tokens via the normal
-- POST /api/tokens flow.

DELETE FROM agent_tokens;

ALTER TABLE agent_tokens RENAME COLUMN token TO token_hash;
ALTER TABLE agent_tokens ADD COLUMN prefix TEXT NOT NULL DEFAULT '';

-- The default above exists only to satisfy NOT NULL for the (now
-- empty) backfill; new inserts always populate prefix explicitly.
ALTER TABLE agent_tokens ALTER COLUMN prefix DROP DEFAULT;

-- Keep the uniqueness property — two tokens can't collide at the
-- hash level (that would mean a sha256 collision) so this is a
-- sanity constraint rather than a user-facing one.
DROP INDEX IF EXISTS idx_agent_tokens_token;
CREATE UNIQUE INDEX idx_agent_tokens_token_hash
    ON agent_tokens(token_hash);

-- +goose Down
ALTER TABLE agent_tokens DROP COLUMN IF EXISTS prefix;
ALTER TABLE agent_tokens RENAME COLUMN token_hash TO token;
DROP INDEX IF EXISTS idx_agent_tokens_token_hash;
CREATE UNIQUE INDEX IF NOT EXISTS idx_agent_tokens_token
    ON agent_tokens(token);
