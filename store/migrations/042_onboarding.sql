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

-- Backfill from the user's oldest active membership.
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

-- Orphan rows (user has no active membership) get assigned to the
-- oldest customer so the NOT NULL constraint can proceed. These are
-- leftover rows from deleted/deactivated users — harmless but must
-- have a valid FK.
UPDATE saved_queries sq
   SET customer_id = (SELECT id FROM customers ORDER BY id ASC LIMIT 1)
 WHERE sq.customer_id IS NULL
   AND EXISTS (SELECT 1 FROM customers);

DELETE FROM saved_queries WHERE customer_id IS NULL;

ALTER TABLE saved_queries ALTER COLUMN customer_id SET NOT NULL;

ALTER TABLE saved_queries
  DROP CONSTRAINT IF EXISTS fk_saved_queries_customer,
  ADD  CONSTRAINT fk_saved_queries_customer
    FOREIGN KEY (customer_id) REFERENCES customers(id) ON DELETE CASCADE;

CREATE INDEX IF NOT EXISTS idx_saved_queries_customer_user
  ON saved_queries(customer_id, user_id);

COMMIT;
