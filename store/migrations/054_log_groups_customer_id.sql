-- +goose Up
-- Denormalize customer_id onto log_groups. The table is customer-owned
-- data but was scoped only indirectly, through
-- log_groups.namespace_id → namespaces.customer_id. A direct column
-- lets log_groups join the enforced tenant-scoping boundary (the
-- customer_id-injecting GORM callback can't reach a column that only
-- exists two joins away). namespace_id stays as the structural FK; the
-- new column is a denormalized copy maintained alongside it.

ALTER TABLE log_groups ADD COLUMN customer_id BIGINT;

UPDATE log_groups lg
SET customer_id = n.customer_id
FROM namespaces n
WHERE lg.namespace_id = n.id;

-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM log_groups WHERE customer_id IS NULL) THEN
    RAISE EXCEPTION 'log_groups rows whose namespace_id resolves to no namespace; aborting before enforcing NOT NULL';
  END IF;
END $$;
-- +goose StatementEnd

ALTER TABLE log_groups ALTER COLUMN customer_id SET NOT NULL;
ALTER TABLE log_groups
  ADD CONSTRAINT fk_log_groups_customer
  FOREIGN KEY (customer_id) REFERENCES customers(id) ON DELETE CASCADE;
CREATE INDEX idx_log_groups_customer ON log_groups (customer_id);

-- Maintain the denormalized column from namespace_id on insert, so no
-- application insert path has to remember to set it (and a forgotten
-- one can't write a NULL/wrong tenant). customer_id is immutable for a
-- given log group — namespace_id never changes — so a BEFORE INSERT
-- fill is sufficient; we don't trigger on UPDATE.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION log_groups_fill_customer_id() RETURNS trigger AS $$
BEGIN
  IF NEW.customer_id IS NULL THEN
    SELECT customer_id INTO NEW.customer_id FROM namespaces WHERE id = NEW.namespace_id;
  END IF;
  RETURN NEW;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

CREATE TRIGGER log_groups_fill_customer_id_bi
  BEFORE INSERT ON log_groups
  FOR EACH ROW EXECUTE FUNCTION log_groups_fill_customer_id();

-- +goose Down
DROP TRIGGER IF EXISTS log_groups_fill_customer_id_bi ON log_groups;
DROP FUNCTION IF EXISTS log_groups_fill_customer_id();
ALTER TABLE log_groups DROP CONSTRAINT IF EXISTS fk_log_groups_customer;
DROP INDEX IF EXISTS idx_log_groups_customer;
ALTER TABLE log_groups DROP COLUMN IF EXISTS customer_id;
