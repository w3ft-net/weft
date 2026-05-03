-- +goose Up
-- Multi-org membership: a user is no longer pinned to one customer
-- via users.customer_id; membership lives in team_members, keyed by
-- a real FK (team_members.customer_id) instead of a name string.
--
-- Migration order matters:
--   1. Add team_members.customer_id; backfill from customer_name; FK.
--   2. Make sure every existing user has a team_members row in their
--      old customer (defensive backfill — the signup flow has always
--      created one, but stale data may exist).
--   3. Pick a canonical user.id per email; remap every user_id FK to
--      the canonical id; delete the duplicates.
--   4. Drop users.customer_id (membership lives in team_members now).
--   5. Add users.last_active_customer_id so reconnects can land where
--      the user left off.
--
-- All steps are idempotent enough that a re-apply on a partial run
-- doesn't double-write or corrupt; the canonical-user step is gated
-- on the duplicate condition so it's a no-op once collapsed.

BEGIN;

-- 1. team_members.customer_id ---------------------------------------------

ALTER TABLE team_members ADD COLUMN IF NOT EXISTS customer_id BIGINT;

UPDATE team_members tm
   SET customer_id = c.id
  FROM customers c
 WHERE c.name = tm.customer_name
   AND tm.customer_id IS NULL;

-- Reject (loudly) any team_members row whose customer_name doesn't
-- resolve. Better to fail the migration than silently lose membership.
-- +goose StatementBegin
DO $$
DECLARE
  orphans bigint;
BEGIN
  SELECT count(*) INTO orphans FROM team_members WHERE customer_id IS NULL;
  IF orphans > 0 THEN
    RAISE EXCEPTION 'team_members has % rows with unresolvable customer_name', orphans;
  END IF;
END$$;
-- +goose StatementEnd

ALTER TABLE team_members ALTER COLUMN customer_id SET NOT NULL;

ALTER TABLE team_members
  DROP CONSTRAINT IF EXISTS fk_team_members_customer,
  ADD  CONSTRAINT fk_team_members_customer
    FOREIGN KEY (customer_id) REFERENCES customers(id) ON DELETE CASCADE;

CREATE INDEX IF NOT EXISTS idx_team_members_customer ON team_members(customer_id);

-- 2. Backfill missing membership rows -------------------------------------
-- Every user has historically been created with a team_members(role=owner)
-- alongside their customer. This insert handles any user that somehow
-- doesn't have one (legacy fixture data, half-completed signup) so step 4
-- doesn't strand them as membership-less after we drop users.customer_id.

INSERT INTO team_members
  (id, customer_id, email, user_id, role, status, invited_at, created_at, updated_at)
SELECT
  gen_random_uuid()::text,
  u.customer_id,
  u.email,
  u.id,
  'owner',
  'active',
  u.created_at,
  u.created_at,
  u.updated_at
  FROM users u
 WHERE NOT EXISTS (
   SELECT 1 FROM team_members tm
    WHERE tm.user_id = u.id AND tm.customer_id = u.customer_id
 );

-- 3. Collapse duplicate user rows -----------------------------------------
-- A user with rows under multiple customers (composite-unique-index
-- holdover) gets unified to the oldest user.id. Every user_id FK is
-- repointed.

CREATE TEMP TABLE _user_canonical AS
SELECT DISTINCT ON (email) email, id AS canonical_id
  FROM users
 ORDER BY email, created_at ASC;

CREATE TEMP TABLE _user_remap AS
SELECT u.id AS old_id, c.canonical_id
  FROM users u
  JOIN _user_canonical c ON c.email = u.email
 WHERE u.id <> c.canonical_id;

-- Repoint every user_id FK in the schema. If a new user_id reference
-- gets added in a future migration, add it to this list too.
UPDATE saved_queries        SET user_id = r.canonical_id FROM _user_remap r WHERE saved_queries.user_id        = r.old_id;
UPDATE namespace_permissions SET user_id = r.canonical_id FROM _user_remap r WHERE namespace_permissions.user_id = r.old_id;
UPDATE team_members          SET user_id = r.canonical_id FROM _user_remap r WHERE team_members.user_id          = r.old_id;
UPDATE device_codes          SET user_id = r.canonical_id FROM _user_remap r WHERE device_codes.user_id          = r.old_id;
UPDATE user_notices          SET user_id = r.canonical_id FROM _user_remap r WHERE user_notices.user_id          = r.old_id;

-- creator-attribution columns (migration 032). notify_configs is
-- intentionally absent — that table never carried a created_by_user_id
-- column despite the original draft of this migration listing it.
UPDATE cron_watches      SET created_by_user_id = r.canonical_id FROM _user_remap r WHERE cron_watches.created_by_user_id      = r.old_id;
UPDATE shell_functions   SET created_by_user_id = r.canonical_id FROM _user_remap r WHERE shell_functions.created_by_user_id   = r.old_id;

-- script_history (migration 036) — both captured_by and authored_by are
-- nullable, but if set they should also be repointed.
UPDATE shell_function_history SET captured_by_user_id = r.canonical_id FROM _user_remap r WHERE shell_function_history.captured_by_user_id = r.old_id;
UPDATE shell_function_history SET authored_by_user_id = r.canonical_id FROM _user_remap r WHERE shell_function_history.authored_by_user_id = r.old_id;
UPDATE cron_watch_history     SET captured_by_user_id = r.canonical_id FROM _user_remap r WHERE cron_watch_history.captured_by_user_id     = r.old_id;
UPDATE cron_watch_history     SET authored_by_user_id = r.canonical_id FROM _user_remap r WHERE cron_watch_history.authored_by_user_id     = r.old_id;

-- Now collapse: delete any users row that isn't the canonical for its
-- email. The team_members rows we backfilled in step 2 (and any from
-- the original signup) keep the membership graph intact.
DELETE FROM users
 WHERE id IN (SELECT old_id FROM _user_remap);

-- After collapse, team_members may now have duplicate (customer_id, user_id)
-- rows that came from different ex-User rows. Keep the oldest active row
-- per (customer_id, user_id), drop the rest.
DELETE FROM team_members tm
 USING (
   SELECT user_id, customer_id, MIN(created_at) AS keep_at
     FROM team_members
    WHERE user_id IS NOT NULL
    GROUP BY user_id, customer_id
   HAVING COUNT(*) > 1
 ) dups
 WHERE tm.user_id     = dups.user_id
   AND tm.customer_id = dups.customer_id
   AND tm.created_at <> dups.keep_at;

-- 4. Drop users.customer_id and customer_name -----------------------------
-- The "user belongs to one customer" assumption is gone. Membership lives
-- in team_members. Drop the FK first so the column can go.

ALTER TABLE users DROP CONSTRAINT IF EXISTS fk_users_customer;
DROP INDEX IF EXISTS idx_user_email_customer;
ALTER TABLE users DROP COLUMN IF EXISTS customer_id;

CREATE UNIQUE INDEX IF NOT EXISTS idx_users_email ON users(email);

-- 5. Last-active customer for reconnect landing ---------------------------

ALTER TABLE users
  ADD COLUMN IF NOT EXISTS last_active_customer_id BIGINT
  REFERENCES customers(id) ON DELETE SET NULL;

-- 6. Drop team_members.customer_name now that customer_id is FK-backed ----

ALTER TABLE team_members DROP COLUMN IF EXISTS customer_name;

COMMIT;
