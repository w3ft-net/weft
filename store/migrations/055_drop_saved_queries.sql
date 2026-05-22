-- +goose Up
-- Remove the vestigial saved_queries feature. The model, store
-- methods, API routes (GET/POST/DELETE /api/queries), and frontend
-- client stubs were Loom-era dead code: the routes were orphaned (no
-- UI caller), and the only live use was an onboarding-graduation sweep
-- that deleted leftover Loom bookmarks. With all of that removed, the
-- table holds nothing reachable.
DROP TABLE IF EXISTS saved_queries;

-- +goose Down
-- Recreate the table shape (data is not restored).
CREATE TABLE IF NOT EXISTS saved_queries (
  id          TEXT PRIMARY KEY,
  customer_id BIGINT NOT NULL,
  user_id     TEXT NOT NULL,
  name        TEXT NOT NULL,
  query       TEXT NOT NULL,
  created_at  TIMESTAMPTZ,
  updated_at  TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_saved_queries_customer_user
  ON saved_queries (customer_id, user_id);
