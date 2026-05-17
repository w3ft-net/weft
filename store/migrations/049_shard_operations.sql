-- +goose Up

-- Append-only audit log of every shard lifecycle state change.
-- Deliberately has NO foreign key on shard_id: the DELETE of a
-- shard_records row must still record its final transition, so the
-- audit row has to outlive the parent. Kept forever (state changes
-- are rare — orders of magnitude below shard_heartbeats — so this is
-- not partman-managed; that also sidesteps the default-partition
-- wedge class entirely). ts uses clock_timestamp() so rows within a
-- single transaction still get distinct, real event times.
CREATE TABLE shard_operations (
    shard_id   text        NOT NULL,
    ts         timestamptz NOT NULL DEFAULT clock_timestamp(),
    from_state text,
    to_state   text
);

CREATE INDEX idx_shard_operations_shard_ts ON shard_operations (shard_id, ts);
CREATE INDEX idx_shard_operations_ts ON shard_operations (ts);

-- One trigger function, no WHEN clause: a WHEN clause can't reference
-- NEW/OLD uniformly across INSERT/UPDATE/DELETE, so the op filter
-- lives in the body. Convention: NULL from_state = created,
-- NULL to_state = deleted. A non-state UPDATE (e.g. the heartbeat
-- row_count touch) records nothing.
-- +goose StatementBegin
CREATE FUNCTION log_shard_operation() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        INSERT INTO shard_operations (shard_id, from_state, to_state)
        VALUES (NEW.id, NULL, NEW.state);
        RETURN NEW;
    ELSIF TG_OP = 'UPDATE' THEN
        IF NEW.state IS DISTINCT FROM OLD.state THEN
            INSERT INTO shard_operations (shard_id, from_state, to_state)
            VALUES (NEW.id, OLD.state, NEW.state);
        END IF;
        RETURN NEW;
    ELSIF TG_OP = 'DELETE' THEN
        INSERT INTO shard_operations (shard_id, from_state, to_state)
        VALUES (OLD.id, OLD.state, NULL);
        RETURN OLD;
    END IF;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

CREATE TRIGGER shard_records_audit
    AFTER INSERT OR UPDATE OR DELETE ON shard_records
    FOR EACH ROW EXECUTE FUNCTION log_shard_operation();

-- +goose Down
DROP TRIGGER IF EXISTS shard_records_audit ON shard_records;
DROP FUNCTION IF EXISTS log_shard_operation();
DROP TABLE IF EXISTS shard_operations;
