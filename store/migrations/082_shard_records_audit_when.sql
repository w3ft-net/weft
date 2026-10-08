-- +goose Up
-- shard_records_audit called log_shard_operation() for every UPDATE of a
-- shard_records row, though the function only records anything when state
-- changed. The heartbeat path updates these rows every few seconds per shard
-- (TouchHeartbeat), so nearly all of those calls were wasted plpgsql
-- invocations. Split the trigger so UPDATE only fires on a state change;
-- INSERT and DELETE keep recording as before.

DROP TRIGGER shard_records_audit ON public.shard_records;

CREATE TRIGGER shard_records_audit_row
    AFTER INSERT OR DELETE ON public.shard_records
    FOR EACH ROW EXECUTE FUNCTION public.log_shard_operation();

CREATE TRIGGER shard_records_audit_state
    AFTER UPDATE ON public.shard_records
    FOR EACH ROW
    WHEN (OLD.state IS DISTINCT FROM NEW.state)
    EXECUTE FUNCTION public.log_shard_operation();

-- +goose Down
DROP TRIGGER shard_records_audit_state ON public.shard_records;
DROP TRIGGER shard_records_audit_row ON public.shard_records;

CREATE TRIGGER shard_records_audit
    AFTER INSERT OR DELETE OR UPDATE ON public.shard_records
    FOR EACH ROW EXECUTE FUNCTION public.log_shard_operation();
