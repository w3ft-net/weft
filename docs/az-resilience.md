# AZ resilience (future work)

Placeholder. Captures future work on availability-zone resilience for the
shard fleet. Not a day-1 concern — recorded here so the shard-lifecycle
doc can point to it without dragging these questions into scope.

## Goals

- **Write shards split across AZs.** A single-AZ impairment should not
  halt ingestion. With N write shards, distribute them across ≥2 AZs so
  the blast radius of an AZ outage is a fraction of the write path, not
  all of it.
- **Cross-AZ snapshotting.** EBS volumes are AZ-local: if the AZ goes
  away, so does the volume. Periodic snapshots (EBS → S3, which is
  regional) let us rebuild a shard in a surviving AZ from the most
  recent snapshot plus whatever the archiver has already uploaded.
- **Read path survives AZ loss.** The coordinator should be able to
  route queries around a shard whose AZ is impaired, serving archived
  (S3) data until the volume is restored elsewhere.

## Rough shape

- Launch templates and ASGs already support multi-AZ; the missing piece
  is EBS: a volume lives in one AZ and cannot attach in another.
  Options: (a) snapshot-then-restore on AZ failover, (b) Multi-Attach
  EBS (io1/io2 only; not obviously a fit), (c) treat AZ failure as a
  full rebuild from S3 archive.
- Snapshots every `w.duration` (or more frequent for the INGESTING
  shard) give bounded RPO.
- Archiver already uploads parquet to S3; the snapshot story layers on
  top of it rather than replacing it.

## Not deciding now

- Whether to pre-split writes across AZs before we have a cross-AZ
  failure plan, or wait until the failover path is implemented.
- Snapshot cadence and retention.
- Cost envelope for cross-AZ EBS snapshots at steady state.

Revisit after durable-identity rollout lands and the fleet is running
in production shape.
