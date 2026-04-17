-- +goose Up
-- Deleting a customer (after cancellation, GDPR delete, or manual cleanup)
-- must cascade to their owned state. Without this, a deleted customer's
-- agent_tokens stay around, weftd keeps streaming rows against the missing
-- customer id, and we end up with ingestion logs like:
--
--   ingest-stream: first row from customer=kevlar app=xylem
--
-- Each ALTER is idempotent (drop-if-exists, then recreate with CASCADE) so
-- this migration can be re-applied safely.

ALTER TABLE namespaces
  DROP CONSTRAINT IF EXISTS fk_customers_namespaces,
  ADD  CONSTRAINT fk_customers_namespaces
    FOREIGN KEY (customer_id) REFERENCES customers(id) ON DELETE CASCADE;

ALTER TABLE accounts
  DROP CONSTRAINT IF EXISTS fk_customers_accounts,
  ADD  CONSTRAINT fk_customers_accounts
    FOREIGN KEY (customer_id) REFERENCES customers(id) ON DELETE CASCADE;

ALTER TABLE agent_tokens
  DROP CONSTRAINT IF EXISTS fk_customers_agent_tokens,
  ADD  CONSTRAINT fk_customers_agent_tokens
    FOREIGN KEY (customer_id) REFERENCES customers(id) ON DELETE CASCADE;

ALTER TABLE users
  DROP CONSTRAINT IF EXISTS fk_users_customer,
  ADD  CONSTRAINT fk_users_customer
    FOREIGN KEY (customer_id) REFERENCES customers(id) ON DELETE CASCADE;

ALTER TABLE pending_sources
  DROP CONSTRAINT IF EXISTS fk_pending_sources_customer,
  ADD  CONSTRAINT fk_pending_sources_customer
    FOREIGN KEY (customer_id) REFERENCES customers(id) ON DELETE CASCADE;

-- Grandchild: deleting a namespace (which itself cascades from customer)
-- must also drop any agent tokens scoped to it.
ALTER TABLE agent_tokens
  DROP CONSTRAINT IF EXISTS fk_agent_tokens_namespace,
  ADD  CONSTRAINT fk_agent_tokens_namespace
    FOREIGN KEY (namespace_id) REFERENCES namespaces(id) ON DELETE CASCADE;

-- Grandchild: deleting an account (cascaded from customer) must also drop
-- its log_groups.
ALTER TABLE log_groups
  DROP CONSTRAINT IF EXISTS fk_accounts_log_groups,
  ADD  CONSTRAINT fk_accounts_log_groups
    FOREIGN KEY (account_id) REFERENCES accounts(id) ON DELETE CASCADE;
