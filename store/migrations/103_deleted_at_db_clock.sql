-- +goose Up
-- Version history orders a deletion by deleted_at and every save by
-- authored_at, which the history triggers stamp with clock_timestamp().
-- deleted_at came from the app's clock, so whenever that ran behind the
-- database's a deletion sorted before saves that happened first. Stamp it
-- from the same clock.
-- +goose StatementBegin
CREATE FUNCTION public.stamp_deleted_at() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF OLD.deleted_at IS NULL AND NEW.deleted_at IS NOT NULL THEN
        NEW.deleted_at := clock_timestamp();
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER stamp_deleted_at_trg BEFORE UPDATE OF deleted_at ON public.cron_watches FOR EACH ROW EXECUTE FUNCTION public.stamp_deleted_at();
CREATE TRIGGER stamp_deleted_at_trg BEFORE UPDATE OF deleted_at ON public.shell_functions FOR EACH ROW EXECUTE FUNCTION public.stamp_deleted_at();
CREATE TRIGGER stamp_deleted_at_trg BEFORE UPDATE OF deleted_at ON public.dashboards FOR EACH ROW EXECUTE FUNCTION public.stamp_deleted_at();
CREATE TRIGGER stamp_deleted_at_trg BEFORE UPDATE OF deleted_at ON public.notify_configs FOR EACH ROW EXECUTE FUNCTION public.stamp_deleted_at();

-- +goose Down
DROP TRIGGER stamp_deleted_at_trg ON public.notify_configs;
DROP TRIGGER stamp_deleted_at_trg ON public.dashboards;
DROP TRIGGER stamp_deleted_at_trg ON public.shell_functions;
DROP TRIGGER stamp_deleted_at_trg ON public.cron_watches;
DROP FUNCTION public.stamp_deleted_at();
