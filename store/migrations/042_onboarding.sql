-- +goose Up
-- Onboarding state + saved_queries org-keying.
--
-- onboarding_step is a text enum (no Postgres enum so we can extend
-- the lesson list without a migration each time). 'welcome' is the
-- default for new signups; 'done' marks a graduated user.
-- onboarding_dismissed lets a user opt out without graduating; the
-- welcome modal and tour overlays read both.
--
-- saved_queries was the lone shell-managed resource still keyed only
-- by user_id (vs customer_id like shell_functions, cron_watches,
-- tmp_files). That made graduation cleanup impossible — onboarding-
-- era queries would leak into the user's own org. Backfill assigns
-- existing rows to the user's oldest active membership (the same
-- default BuildSession picks when no X-Active-Org is set).

BEGIN;

-- 1. Onboarding state on users -------------------------------------------

ALTER TABLE users
  ADD COLUMN IF NOT EXISTS onboarding_step       TEXT  NOT NULL DEFAULT 'welcome',
  ADD COLUMN IF NOT EXISTS onboarding_dismissed  BOOL  NOT NULL DEFAULT FALSE;

-- 2. saved_queries.customer_id -------------------------------------------

ALTER TABLE saved_queries ADD COLUMN IF NOT EXISTS customer_id BIGINT;

-- Backfill from the user's oldest active membership. A user with zero
-- memberships shouldn't exist after the multi-org migration, but if
-- one does, their saved_queries get null customer_id and the NOT NULL
-- constraint below will fail loudly — better than silent loss.
UPDATE saved_queries sq
   SET customer_id = (
     SELECT tm.customer_id
       FROM team_members tm
      WHERE tm.user_id = sq.user_id
        AND tm.status = 'active'
      ORDER BY tm.created_at ASC
      LIMIT 1
   )
 WHERE sq.customer_id IS NULL;

DO $$
DECLARE
  orphans bigint;
BEGIN
  SELECT count(*) INTO orphans FROM saved_queries WHERE customer_id IS NULL;
  IF orphans > 0 THEN
    RAISE EXCEPTION 'saved_queries has % rows with no resolvable customer (user has no membership)', orphans;
  END IF;
END$$;

ALTER TABLE saved_queries ALTER COLUMN customer_id SET NOT NULL;

ALTER TABLE saved_queries
  DROP CONSTRAINT IF EXISTS fk_saved_queries_customer,
  ADD  CONSTRAINT fk_saved_queries_customer
    FOREIGN KEY (customer_id) REFERENCES customers(id) ON DELETE CASCADE;

CREATE INDEX IF NOT EXISTS idx_saved_queries_customer_user
  ON saved_queries(customer_id, user_id);

COMMIT;
