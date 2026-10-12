-- +goose Up
-- Version history for /etc/dashboards/*.yaml and /etc/notify.yaml, shaped
-- like the /bin and /cron.d history (085-087) so the git view can cover
-- them: a stable version_id, authored_at/authored_by, origin_version_id (to
-- tell a trimmed history from a first version), a commit message, and a
-- history table holding the newest five replaced versions.
--
-- Dashboards are soft-deleted and the unique name index is partial
-- (deleted_at IS NULL), so saving a deleted name creates a new row; the old
-- row keeps its deleted_at. notify_configs is one row per org that is never
-- deleted; only aliases_yaml (the file) is versioned, not the Slack columns.

ALTER TABLE public.dashboards
    ADD COLUMN version_id bigint,
    ADD COLUMN authored_at timestamptz,
    ADD COLUMN authored_by_user_id text,
    ADD COLUMN origin_version_id bigint,
    ADD COLUMN message text NOT NULL DEFAULT '';
ALTER TABLE public.notify_configs
    ADD COLUMN version_id bigint,
    ADD COLUMN authored_at timestamptz,
    ADD COLUMN authored_by_user_id text,
    ADD COLUMN origin_version_id bigint,
    ADD COLUMN message text NOT NULL DEFAULT '';

-- No history existed, so every row is on its first version.
UPDATE public.dashboards SET
    version_id = nextval('public.object_version_seq'),
    authored_at = COALESCE(updated_at, created_at, now()),
    authored_by_user_id = created_by_user_id;
UPDATE public.dashboards SET origin_version_id = version_id;
UPDATE public.notify_configs SET
    version_id = nextval('public.object_version_seq'),
    authored_at = COALESCE(updated_at, created_at, now());
UPDATE public.notify_configs SET origin_version_id = version_id;

ALTER TABLE public.dashboards
    ALTER COLUMN version_id SET DEFAULT nextval('public.object_version_seq'),
    ALTER COLUMN version_id SET NOT NULL,
    ALTER COLUMN authored_at SET DEFAULT clock_timestamp(),
    ALTER COLUMN authored_at SET NOT NULL,
    ALTER COLUMN origin_version_id SET NOT NULL;
ALTER TABLE public.notify_configs
    ALTER COLUMN version_id SET DEFAULT nextval('public.object_version_seq'),
    ALTER COLUMN version_id SET NOT NULL,
    ALTER COLUMN authored_at SET DEFAULT clock_timestamp(),
    ALTER COLUMN authored_at SET NOT NULL,
    ALTER COLUMN origin_version_id SET NOT NULL;
CREATE UNIQUE INDEX idx_dashboards_version_id ON public.dashboards (version_id);
CREATE UNIQUE INDEX idx_notify_configs_version_id ON public.notify_configs (version_id);

CREATE TABLE public.dashboard_history (
    id bigserial PRIMARY KEY,
    dashboard_id bigint NOT NULL,
    org_id bigint NOT NULL,
    name text NOT NULL,
    yaml_body text NOT NULL,
    message text NOT NULL DEFAULT '',
    captured_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    captured_by_user_id text,
    authored_by_user_id text,
    predecessor_id bigint REFERENCES public.dashboard_history(id) ON DELETE SET NULL,
    version_id bigint NOT NULL,
    authored_at timestamptz NOT NULL
);
CREATE INDEX idx_dh_dashboard_id ON public.dashboard_history (dashboard_id, captured_at DESC);
CREATE UNIQUE INDEX idx_dh_version_id ON public.dashboard_history (version_id);

CREATE TABLE public.notify_config_history (
    id bigserial PRIMARY KEY,
    notify_config_id bigint NOT NULL,
    org_id bigint NOT NULL,
    aliases_yaml text NOT NULL,
    message text NOT NULL DEFAULT '',
    captured_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    captured_by_user_id text,
    authored_by_user_id text,
    predecessor_id bigint REFERENCES public.notify_config_history(id) ON DELETE SET NULL,
    version_id bigint NOT NULL,
    authored_at timestamptz NOT NULL
);
CREATE INDEX idx_nch_config_id ON public.notify_config_history (notify_config_id, captured_at DESC);
CREATE UNIQUE INDEX idx_nch_version_id ON public.notify_config_history (version_id);

-- +goose StatementBegin
CREATE FUNCTION public.etc_set_origin_on_insert() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    NEW.origin_version_id := NEW.version_id;
    NEW.authored_by_user_id := NULLIF(current_setting('weft.acting_user_id', true), '');
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
CREATE TRIGGER dashboards_origin_trg BEFORE INSERT ON public.dashboards
    FOR EACH ROW EXECUTE FUNCTION public.etc_set_origin_on_insert();
CREATE TRIGGER notify_configs_origin_trg BEFORE INSERT ON public.notify_configs
    FOR EACH ROW EXECUTE FUNCTION public.etc_set_origin_on_insert();

-- +goose StatementBegin
CREATE FUNCTION public.capture_dashboard_history() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    last_history_id BIGINT;
    captured_by     TEXT;
BEGIN
    IF OLD.yaml_body = NEW.yaml_body THEN
        RETURN NEW;
    END IF;

    captured_by := NULLIF(current_setting('weft.acting_user_id', true), '');

    SELECT id INTO last_history_id FROM dashboard_history
     WHERE dashboard_id = OLD.id ORDER BY captured_at DESC, id DESC LIMIT 1;

    INSERT INTO dashboard_history (
        dashboard_id, org_id, name, yaml_body, message, captured_by_user_id,
        authored_by_user_id, predecessor_id, version_id, authored_at
    ) VALUES (
        OLD.id, OLD.org_id, OLD.name, OLD.yaml_body, OLD.message, captured_by,
        OLD.authored_by_user_id, last_history_id, OLD.version_id, OLD.authored_at
    );

    NEW.version_id := nextval('public.object_version_seq');
    NEW.authored_at := clock_timestamp();
    NEW.authored_by_user_id := captured_by;
    NEW.message := '';

    IF (SELECT count(*) FROM dashboard_history WHERE dashboard_id = OLD.id) > 5 THEN
        DELETE FROM dashboard_history WHERE id = (
            SELECT id FROM dashboard_history WHERE dashboard_id = OLD.id
             ORDER BY captured_at ASC, id ASC LIMIT 1);
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
CREATE TRIGGER capture_dashboard_history_trg BEFORE UPDATE OF yaml_body ON public.dashboards
    FOR EACH ROW EXECUTE FUNCTION public.capture_dashboard_history();

-- +goose StatementBegin
CREATE FUNCTION public.capture_notify_config_history() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    last_history_id BIGINT;
    captured_by     TEXT;
BEGIN
    IF OLD.aliases_yaml = NEW.aliases_yaml THEN
        RETURN NEW;
    END IF;

    captured_by := NULLIF(current_setting('weft.acting_user_id', true), '');

    SELECT id INTO last_history_id FROM notify_config_history
     WHERE notify_config_id = OLD.id ORDER BY captured_at DESC, id DESC LIMIT 1;

    INSERT INTO notify_config_history (
        notify_config_id, org_id, aliases_yaml, message, captured_by_user_id,
        authored_by_user_id, predecessor_id, version_id, authored_at
    ) VALUES (
        OLD.id, OLD.org_id, OLD.aliases_yaml, OLD.message, captured_by,
        OLD.authored_by_user_id, last_history_id, OLD.version_id, OLD.authored_at
    );

    NEW.version_id := nextval('public.object_version_seq');
    NEW.authored_at := clock_timestamp();
    NEW.authored_by_user_id := captured_by;
    NEW.message := '';

    IF (SELECT count(*) FROM notify_config_history WHERE notify_config_id = OLD.id) > 5 THEN
        DELETE FROM notify_config_history WHERE id = (
            SELECT id FROM notify_config_history WHERE notify_config_id = OLD.id
             ORDER BY captured_at ASC, id ASC LIMIT 1);
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
CREATE TRIGGER capture_notify_config_history_trg BEFORE UPDATE OF aliases_yaml ON public.notify_configs
    FOR EACH ROW EXECUTE FUNCTION public.capture_notify_config_history();
