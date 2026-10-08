-- +goose Up
-- shard_heartbeats becomes a plain table holding the latest beat per source,
-- instead of an hourly-partitioned log of every beat.
--
-- Nothing reads heartbeat history: the provisioner's catch-up sweep looks back
-- one minute, and the only unbounded reader (export sizing) moves to
-- shard_records.disk_bytes below. The partitioned form needed partman to keep
-- creating partitions; once maintenance started failing (a stale partition's
-- DROP deadlocked against live traffic, and rows in the default partition
-- blocked every ATTACH) every beat landed in shard_heartbeats_default, which
-- grew to ~1 GB and was seq-scanned by the 60s adopt query.
--
-- Rows are keyed by (shard_id, instance_id): a claimed shard has one row per
-- instance it has run on (the old instance's row ages out via
-- PruneShardHeartbeats), and unclaimed instances beat with shard_id = ''.

ALTER TABLE public.shard_records
    ADD COLUMN disk_bytes bigint NOT NULL DEFAULT 0;

-- Carry each shard's last reported footprint over before the old table goes,
-- so a shard that is ARCHIVING at deploy time still sizes its export host.
UPDATE public.shard_records r
SET disk_bytes = h.disk_bytes
FROM (
    SELECT DISTINCT ON (shard_id) shard_id, disk_bytes
    FROM public.shard_heartbeats
    WHERE disk_bytes > 0
    ORDER BY shard_id, beat_at DESC
) h
WHERE r.id = h.shard_id;

-- +goose StatementBegin
DO $mig$
BEGIN
    IF to_regclass('partman.part_config') IS NOT NULL THEN
        DELETE FROM partman.part_config WHERE parent_table = 'public.shard_heartbeats';
    END IF;
END
$mig$;
-- +goose StatementEnd

DROP TABLE IF EXISTS partman.template_public_shard_heartbeats;
DROP TABLE public.shard_heartbeats;

CREATE TABLE public.shard_heartbeats (
    shard_id    text NOT NULL,
    instance_id text NOT NULL DEFAULT '',
    addr        text NOT NULL,
    state       text NOT NULL,
    row_count   bigint NOT NULL DEFAULT 0,
    disk_bytes  bigint NOT NULL DEFAULT 0,
    beat_at     timestamptz NOT NULL,
    PRIMARY KEY (shard_id, instance_id)
);

CREATE INDEX shard_heartbeats_beat_at_idx ON public.shard_heartbeats (beat_at);

-- +goose Down
-- Recreates the unpartitioned shape only; the partman registration is not
-- restored (it was already failing in prod, and the data is ephemeral).
DROP TABLE public.shard_heartbeats;
CREATE TABLE public.shard_heartbeats (
    shard_id    text NOT NULL,
    addr        text NOT NULL,
    instance_id text,
    state       text NOT NULL,
    row_count   bigint NOT NULL DEFAULT 0,
    disk_bytes  bigint NOT NULL DEFAULT 0,
    beat_at     timestamptz NOT NULL,
    PRIMARY KEY (shard_id, beat_at)
);
ALTER TABLE public.shard_records DROP COLUMN IF EXISTS disk_bytes;
