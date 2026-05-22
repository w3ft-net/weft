-- +goose Up
-- Re-key subscriptions on the customers surrogate id instead of the
-- customer name. Every other tenant-scoped table FKs customers(id);
-- subscriptions was the lone holdout storing the name string, which
-- blocked a uniform tenant-scoping mechanism. See the tenant-isolation
-- work this migration precedes.
--
-- Sequence: add the FK column, backfill from name → id, fail closed if
-- any subscription's customer_id resolves to no customer, then drop the
-- string column and promote the FK column to the canonical name.

ALTER TABLE subscriptions ADD COLUMN customer_fk BIGINT;

UPDATE subscriptions s
SET customer_fk = c.id
FROM customers c
WHERE s.customer_id = c.name;

-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM subscriptions WHERE customer_fk IS NULL) THEN
    RAISE EXCEPTION 'subscription rows with unresolvable customer_id (no matching customers.name); aborting migration to avoid orphaning billing data';
  END IF;
END $$;
-- +goose StatementEnd

ALTER TABLE subscriptions DROP COLUMN customer_id;
ALTER TABLE subscriptions RENAME COLUMN customer_fk TO customer_id;
ALTER TABLE subscriptions ALTER COLUMN customer_id SET NOT NULL;
ALTER TABLE subscriptions
  ADD CONSTRAINT fk_subscriptions_customer
  FOREIGN KEY (customer_id) REFERENCES customers(id) ON DELETE CASCADE;
CREATE INDEX idx_subscriptions_customer ON subscriptions (customer_id);

-- +goose Down
-- Reverse: re-add the name string column, backfill from the FK, drop
-- the FK column. Down-migrations are best-effort dev conveniences;
-- prod rolls forward.
ALTER TABLE subscriptions DROP CONSTRAINT IF EXISTS fk_subscriptions_customer;
DROP INDEX IF EXISTS idx_subscriptions_customer;

ALTER TABLE subscriptions ADD COLUMN customer_name TEXT;

UPDATE subscriptions s
SET customer_name = c.name
FROM customers c
WHERE s.customer_id = c.id;

ALTER TABLE subscriptions DROP COLUMN customer_id;
ALTER TABLE subscriptions RENAME COLUMN customer_name TO customer_id;
ALTER TABLE subscriptions ALTER COLUMN customer_id SET NOT NULL;
CREATE INDEX idx_subscriptions_customer ON subscriptions (customer_id);
