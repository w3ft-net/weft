-- +goose Up
-- Customer-scoped named dashboards. Body is the YAML the user authored
-- under /etc/dashboards/<name>.conf — kept as text and re-parsed on read
-- so the shell and the API see exactly what was written.

CREATE TABLE public.dashboards (
    id bigserial PRIMARY KEY,
    created_at timestamp with time zone,
    updated_at timestamp with time zone,
    deleted_at timestamp with time zone,
    customer_id bigint NOT NULL,
    name text NOT NULL,
    yaml_body text NOT NULL,
    created_by_user_id text
);

CREATE UNIQUE INDEX idx_dashboards_customer_name
    ON public.dashboards (customer_id, name)
    WHERE deleted_at IS NULL;

CREATE INDEX idx_dashboards_deleted_at ON public.dashboards (deleted_at);

-- +goose Down
DROP TABLE public.dashboards;
