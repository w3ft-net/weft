-- +goose Up
-- App buckets: groupings of apps that share a shard fleet. Each bucket
-- declares the target number of INGESTING shards it expects, the tier
-- (instance class / ASG) those shards run on, and the apps it covers
-- (empty/'[]' = catch-all, which is the day-1 'default' bucket).
--
-- target_ingesting_count drives the bucket-sizing loop in
-- provisioner.check(); it replaces the IngestingPerBucket field on
-- ProvisionerConfig and the implicit reliance on Terraform's
-- shard_desired for "how many write shards should exist."
--
-- shard_records.bucket_id is a hard FK so every shard always names
-- the bucket that owns it; reads can fan out by bucket without a
-- separate lookup. Pre-existing rows survived migration 019 only
-- if they were on a real (non-loopback) addr with a sane window —
-- those are catch-all by construction (christen has been writing
-- Apps: nil since rev 1), so backfilling them to 'default' is
-- accurate, not aspirational.

CREATE TABLE IF NOT EXISTS buckets (
    id                     text NOT NULL PRIMARY KEY,
    apps                   text NOT NULL,
    target_ingesting_count int  NOT NULL,
    tier                   text NOT NULL DEFAULT 'small',
    created_at             text NOT NULL
);

INSERT INTO buckets (id, apps, target_ingesting_count, tier, created_at)
VALUES (
    'default',
    '[]',
    2,
    'small',
    to_char(now() AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"')
)
ON CONFLICT (id) DO NOTHING;

ALTER TABLE shard_records
    ADD COLUMN IF NOT EXISTS bucket_id text NOT NULL DEFAULT 'default'
    REFERENCES buckets(id);

-- +goose Down
-- Destructive — buckets aren't recoverable from the FK side. No-op.
