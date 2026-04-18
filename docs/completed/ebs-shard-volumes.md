# Plan: EBS per-shard data volumes

Gap item #4 in `docs/shard-lifecycle.md`. Standalone plan because this is
the invasive change that turns shards from "instance-bound" into "volume-
bound" entities — it touches coordinator Go code, the shard boot
sequence, the shard ASGs' launch templates, and IAM.

Status: **approved, no code yet.** All decisions resolved — see "Decisions" below.

## Goal

Today's shard loses its DuckDB the moment the EC2 instance dies. The
target: a shard's DuckDB lives on a dedicated EBS volume tagged with the
shard id; the volume outlives the instance and gets re-attached to a
replacement instance on recovery. Same shard id, same window, same
rows — the instance is just a compute host that happens to be holding
the volume right now.

## What exists today

- **Launch templates (shard ASGs)**: standard `asg-application` module.
  Root volume only via AMI default. No data-volume block mapping. Good
  starting point — it's already "naked" in the sense the gap analysis
  wants.
- **Shard boot**: `deploy/startup.sh` + `weft-shard.service`. Opens
  DuckDB at `/opt/app/data/shard.duckdb`, which today lives on the root
  volume. No wait-for-device logic, no mount step beyond what cloud-init
  provides for the root volume.
- **ASGLauncher** (`coordinator/launcher_asg.go`): `LaunchInstance`
  bumps desired capacity, polls for a new InService instance.
  `TerminateInstance` calls
  `TerminateInstanceInAutoScalingGroup(ShouldDecrementDesiredCapacity=true)`.
  No volume awareness anywhere.
- **IAM (coordinator role)**: has `autoscaling:*` for ASG ops,
  `ec2:DescribeInstances`. **No** `ec2:CreateVolume`, `AttachVolume`,
  `DetachVolume`, `DescribeVolumes`, or `CreateTags`.
- **`shard_records` columns**: no `ebs_volume_id` (gap item #4c).

## The three events we have to handle

1. **Christening**: brand-new fleet or scale-up. Coordinator needs to
   provision a volume, attach it, tell the shard to format/mount.
2. **Recovery**: the EC2 instance hosting shard S dies; a replacement
   comes up; coordinator has to move S's existing EBS from the dead
   instance to the new one.
3. **Retirement**: shard is ARCHIVED and no longer needed hot. Volume
   gets detached, deleted (or snapshotted first; see
   `docs/az-resilience.md` for the snapshot angle).

Everything else (writes, queries, heartbeats) is volume-agnostic once
the mount exists.

## Key design decisions

### D1 — Who creates the volume?  →  **coordinator**

The shard process itself can't usefully create a volume: it has no IAM
for `ec2:CreateVolume`, and giving it that permission widens the
blast radius if a shard is compromised. The coordinator already has
privileged AWS creds; adding the volume APIs is a small delta.

Implication: recovery and christening both flow through the coordinator.
That matches the existing "coordinator is the sole lifecycle controller"
invariant in the doc.

### D2 — How does the shard process learn about the volume?  →  **ASG lifecycle hook with Pending:Wait** (decided)

Shard ASG has an `EC2_INSTANCE_LAUNCHING` lifecycle hook. New instance
sits in Pending:Wait. Coordinator (subscribed via SQS) decides whether
this is a christening (new volume) or recovery (existing volume), does
the attach, then signals CompleteLifecycleAction. Cloud-init runs
after the signal and sees the attached volume. Shard boots cleanly.

No race: device is always there by the time the shard process starts.
Matches the `migration_command` lifecycle hook pattern already proven
on the coordinator ASG.

Trade-off accepted: if the coordinator is down, new shards stall in
Pending:Wait until the hook timeout. No worse than today — nothing
comes up without a coordinator anyway.

Pros: no lifecycle hook. Coordinator doesn't have to subscribe to
anything. Shard can tolerate the coordinator being flappy for longer.

### D3 — How does the coordinator decide christen vs. recover on a launch?  →  **AZ + apps + "any volume tagged for this fleet without a live instance"**

When a new instance shows up (Pending:Wait), the
coordinator has to decide: is this a fresh christening (need new
volume), or is this replacing a dead instance for some existing shard?

The decision:
1. List volumes tagged with `shard_fleet=<tier>` and
   `shard_id=<some-id>` in the new instance's AZ.
2. For each volume, check `shard_records` — is its shard state still
   INGESTING or READ_ONLY, and is the instance last known to host it
   dead (no heartbeat in N minutes)?
3. If exactly one such volume exists → **recovery**: attach it.
4. If zero → **christening**: create a new volume, tag it, attach.
5. If more than one → **alarm**. Shouldn't happen; we should refuse to
   act and surface it as an operational issue rather than guess.

The AZ constraint is non-negotiable: EBS is AZ-scoped.

### D4 — Device naming  →  `/dev/sdf` (with nvme rename tolerance)

The launch-template attach specifies `/dev/sdf`. On nitro instances
the kernel renames it `/dev/nvme1n1` or similar; the mount script
needs to handle both. Our current AMIs include `ec2-utils` /
`nvme-cli` which provide the mapping.

Filesystem: ext4. Simple, well-understood, grows cleanly via
`resize2fs` if we ever need to grow a shard.

Mount point: `/opt/app/data` (matches today's path for the shard data
directory — one less thing to change). Mount options:
`defaults,noatime` (duckdb does its own journaling; atime is wasted
IO).

### D5 — Volume size  →  **fixed per tier, configurable at fleet level**

Tier-aware: small/medium/large fleets already exist; extend with
`data_volume_size_gb` per tier (default: small=100, medium=500,
large=2000). gp3 IOPS default (3000) is fine for MVP.

These get big — grown, not shrunk — so start conservative. Day-1 we
can tune without touching the coordinator code.

### D6 — Retirement  →  **snapshot → detach → delete** (decided)

When a shard reaches ARCHIVED:
1. `CreateSnapshot` on the data volume, tagged with `shard_id`,
   `archived_at`, and the parquet path already in `shard_records`.
   Wait for snapshot to reach `completed`.
2. `DetachVolume`.
3. `DeleteVolume`.

The snapshot is a cheap backstop against "oops, we archived wrong"
— if an archive turns out to be broken, we can restore the volume
rather than lose the data forever. Retention of these snapshots is
a separate conversation; for now they live indefinitely and we'll
add a lifecycle policy when cost becomes meaningful.

Cross-AZ / DR snapshotting for *live* shards is a separate problem
— see `docs/az-resilience.md`.

## Sub-task breakdown

In the order I'd land them. Each is independently shippable.

### S1 — Terraform: data-volume schema + IAM

- Add `data_volume_size_gb` and `data_volume_device_name` to the
  `asg-application` module.
- Extend coordinator IAM policy: `ec2:DescribeVolumes`,
  `ec2:AttachVolume`, `ec2:DetachVolume`, `ec2:CreateVolume`,
  `ec2:DeleteVolume`, `ec2:CreateTags` (D1 decision).
- Add `data_volume_size_gb` variable per tier in the
  `asg-weft-application` stack.
- **Do not** change the shard launch template yet — we're still
  root-only until S4.
- Apply to prod. No-op at runtime (new variables default to 0 /
  disabled).

Test: `terraform plan` shows only IAM + variable additions, no
destructive changes.

### S2 — shard_records.ebs_volume_id column

Migration 009 adds `ebs_volume_id TEXT` to `shard_records`. GORM
model + `ShardRecord` struct get the field. `Registry.persist()`
writes it. `LoadFromStore` reads it.

Nothing populates it yet; this just makes the column exist.

### S3 — Coordinator: volume lifecycle helpers

New file `coordinator/volumes.go`. Small, testable:

```go
type VolumeManager interface {
    CreateForShard(ctx, shardID, tier, az string) (volumeID string, err error)
    FindByShard(ctx, shardID string) (volumeID, az string, err error)
    AttachToInstance(ctx, volumeID, instanceID, device string) error
    DetachFromInstance(ctx, volumeID string, force bool) error
    DeleteVolume(ctx, volumeID string) error
}
```

With an `ec2VolumeManager` implementation over aws-sdk-go-v2 and a
`fakeVolumeManager` for unit tests. Pure wrappers; no decision logic
here.

Integration test with moto or a real EC2 not in scope — unit coverage
via the fake is enough for the policy logic in later steps.

### S4 — Shard boot: mount `/dev/sdf` on startup

Update `deploy/startup.sh`:
1. If `$ROLE = shard`, wait up to 60s for a block device (by label
   via `ec2-utils`, or by path `/dev/sdf` + nvme resolution) to appear.
2. If unformatted (probe via `blkid`): `mkfs.ext4 -L weft-data`.
3. Mount at `/opt/app/data` with `defaults,noatime`.
4. Fall through to shard start regardless — if no device appears, the
   shard still comes up on the root volume (dev/container mode).

Testable in docker via a named-volume bind mount; prod via a manual
shard launch.

### S5 — ASG lifecycle hook on shard ASGs (D2-A)

Terraform:
- Add `EC2_INSTANCE_LAUNCHING` lifecycle hook to each shard ASG.
  Publish to an SNS topic per fleet.
- Coordinator subscribes (via SQS queue per fleet, polled by a new
  goroutine).

Coordinator logic:
- On lifecycle-hook message: resolve the instance's AZ, decide
  christen vs. recover (D3), attach or create+attach the volume,
  call `CompleteLifecycleAction` with CONTINUE.
- On failure: `CompleteLifecycleAction` with ABANDON. The ASG
  terminates the pending instance and launches a replacement.

Test:
- Unit: hook-message handler + VolumeManager fake, assert the right
  sequence of calls for each of the three scenarios (new fleet, new
  instance-for-existing-shard, orphan-volume).
- Prod smoke: kill a shard instance, watch replacement come up with
  the same shard id.

### S6 — Archive → snapshot → detach → delete path

Extend the existing archive flow in `provisioner.archive()`:
- After `StateArchived` transition and Parquet export complete:
  `VolumeManager.CreateSnapshot` (tagged with shard_id + parquet_path)
  → wait for `completed` → `DetachFromInstance` → `DeleteVolume`.
- Update `shard_records.ebs_volume_id` to empty.
- Terminate the instance via the existing ASG path.

## Rollout order

Since we're pre-launch, the existing shard fleets (and their data)
can be blown away at cutover. No per-fleet feature flag — the
migration is the terraform apply plus a coordinator deploy, and any
legacy shards die with the old launch template. Simpler than a
flagged rollout, appropriate for where we are.

1. **S1** (terraform schema + IAM) — inert at runtime.
2. **S2** (migration) — inert at runtime.
3. **S3** (VolumeManager helpers + unit tests) — inert.
4. **S4** (shard mount logic) — dev-compose + manual prod shard
   verification. Still safe: if no data volume, nothing changes.
5. **S5** (lifecycle hook + christening + recovery) — behavior
   change. Ship to all fleets at once; existing shards get blown
   away on the first ASG refresh.
6. **S6** (snapshot + retirement) — behavior change on the archive
   path.

## Things explicitly not in scope

- Cross-AZ snapshotting / AZ resilience (see
  `docs/az-resilience.md`).
- Per-app or dedicated-fleet shards (see ambiguities section in
  shard-lifecycle.md).
- Dynamic volume resize. Grow-in-place via `modify-volume` is cheap
  to add later; not worth the complexity now.
- Shard-to-shard volume reassignment (e.g. "move this shard from the
  small fleet to the large fleet"). Different problem.

## Test strategy

- **Unit (`coordinator/volumes_test.go`, `coordinator/lifecycle_test.go`)**:
  VolumeManager fake + assertion on coordinator decision tree.
- **Integration (`integration/`)**: docker-compose with named
  volumes standing in for EBS; exercise christening and restart-
  preserves-identity flows. Harder to exercise cross-instance
  recovery in docker since we don't have a real ASG — that stays as
  prod smoke.
- **Prod smoke** (once): launch a shard, write some rows, kill the
  instance, confirm the replacement comes up with the same shard id
  and the rows survive.

## Decisions (for quick reference)

- **D1** — coordinator owns volume creation.
- **D2** — ASG lifecycle hook (Pending:Wait) over shard-side polling.
- **D3** — "one dead-instance volume tagged for this fleet in this
  AZ" ⇒ recover; zero ⇒ christen; multiple ⇒ alarm.
- **D4** — `/dev/sdf`, ext4 `weft-data`, mounted at `/opt/app/data`,
  `defaults,noatime`.
- **D5** — per-tier `data_volume_size_gb` (small=100, medium=500,
  large=2000), gp3 defaults.
- **D6** — snapshot → detach → delete on archive. Snapshots tagged
  with shard_id and parquet path; retention policy deferred.
- **Rollout** — no feature flag; existing shards blow away with the
  launch-template change.
