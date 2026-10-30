-- +goose Up
-- The field facts a shard has reported (docs/plans/field-types.md): which awk
-- fields of which source held only numbers in every row it ingested. Only
-- TYPED facts have a row; a locator that is dirty or never typed is absence,
-- which the planner reads as "unknown". Loaded at boot with the shard
-- registry so a restarted or second coordinator knows them without waiting
-- on heartbeats, and gone with the shard's record.
CREATE TABLE public.shard_field_facts (
    shard_id    text   NOT NULL REFERENCES public.shard_records(id) ON DELETE CASCADE,
    customer_id bigint NOT NULL,
    namespace   text   NOT NULL,
    service     text   NOT NULL,
    kind        text   NOT NULL,
    env         text   NOT NULL,
    locator     text   NOT NULL,
    epoch       bigint NOT NULL,
    PRIMARY KEY (shard_id, customer_id, namespace, service, kind, env, locator)
);

-- +goose Down
DROP TABLE IF EXISTS public.shard_field_facts;
