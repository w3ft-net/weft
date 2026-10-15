-- +goose Up
-- Which agent versions are reading each source. weftd parses client-side, so
-- whether a schema can be interpreted depends on the agent; the API checks
-- the oldest recently-seen agent before storing a schema that needs a newer
-- one. One row per (source, host), refreshed as the agent polls.
CREATE TABLE public.log_group_agents (
    log_group_id bigint NOT NULL REFERENCES public.log_groups(id) ON DELETE CASCADE,
    instance text NOT NULL,
    version text NOT NULL,
    seen_at timestamp with time zone NOT NULL DEFAULT now(),
    PRIMARY KEY (log_group_id, instance)
);

-- +goose Down
DROP TABLE public.log_group_agents;
