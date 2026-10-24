-- +goose Up
-- Seamless shard upgrade (docs/plans/shard-seamless-upgrade.md).
--
-- shard_spares is the warm pool: booted, volume-less shard instances, one per
-- (tier, AZ) some bucket or live shard uses, that an upgrade, a recovery, or a
-- mint claims instead of waiting on EC2. The FSM leader reconciles it against
-- EC2 every tick; a claim is one conditional UPDATE (claimed_by), so two paths
-- can never take the same spare. No FK on claimed_by: a shard that goes GONE
-- deletes its row, and the reconciler terminates whatever it still held.
CREATE TABLE public.shard_spares (
    instance_id text PRIMARY KEY,
    tier text NOT NULL,
    az text NOT NULL,
    image_id text DEFAULT ''::text NOT NULL,
    addr text DEFAULT ''::text NOT NULL,
    binary_sha text DEFAULT ''::text NOT NULL,
    launched_at timestamp with time zone NOT NULL,
    last_heartbeat_at timestamp with time zone,
    claimed_by text,
    claimed_at timestamp with time zone
);

CREATE INDEX idx_shard_spares_unclaimed ON public.shard_spares USING btree (tier, az) WHERE claimed_by IS NULL;

-- UPGRADING is a serving state with persisted phases. Every phase re-derives
-- from these columns after a restart or a leader change. upgrade_origin is
-- the state the upgrade returns to (and what write routing treats the shard
-- as meanwhile); the successor is the claimed spare; the demoted instance is
-- the one the shard is leaving, kept as the rollback target until the
-- upgrade completes.
ALTER TABLE public.shard_records
    ADD COLUMN upgrade_phase text,
    ADD COLUMN upgrade_origin text,
    ADD COLUMN upgrade_phase_at timestamp with time zone,
    ADD COLUMN upgrade_successor_instance_id text,
    ADD COLUMN upgrade_successor_addr text,
    ADD COLUMN upgrade_demoted_instance_id text,
    ADD COLUMN upgrade_demoted_addr text;

-- +goose Down
ALTER TABLE public.shard_records
    DROP COLUMN upgrade_phase,
    DROP COLUMN upgrade_origin,
    DROP COLUMN upgrade_phase_at,
    DROP COLUMN upgrade_successor_instance_id,
    DROP COLUMN upgrade_successor_addr,
    DROP COLUMN upgrade_demoted_instance_id,
    DROP COLUMN upgrade_demoted_addr;

DROP TABLE public.shard_spares;
