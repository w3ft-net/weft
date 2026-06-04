# Coordinator-managed EC2 shards (no ASG)

The coordinator launches and terminates shard instances directly via the EC2
API. There is no Auto Scaling Group in the shard data path. This supersedes the
ASG launch primitive in [`asg-as-api-stateless.md`](asg-as-api-stateless.md);
the stateless shard/bucket FSM design there is otherwise unchanged.

## Why

Treating the per-(tier, AZ) ASG as a launch API created **two capacity
authorities**: the coordinator moved `desired_capacity` to launch a shard, and
the ASG independently maintained that desired count. When an instance died
out-of-band (EC2/ELB health check, AZ rebalance, instance refresh) the ASG
relaunched a replacement the coordinator never asked for — bare, with no
`shard_id` tag and no data volume — while the coordinator's own recovery
launched *its* tracked replacement on top. The ASG's relaunch was never claimed
or reaped (only volumes had tag-based recovery; instances did not, and there was
no reaper). Result observed in prod: 14 instances for 6 shards, 8 of them ghosts,
with `desired == running` ratcheted up.

The architecture's premise — "each shard's own `Evaluate` drives convergence,
there's no separate reconciler" — was false while the ASG existed: the ASG *was*
a second reconciler. Removing it makes the premise true and the ghost class
structurally impossible: nothing but the coordinator can create a shard instance.

## Design

`EC2Provisioner` (`logdb/provisioner_ec2.go`) implements the same
`ShardLaunchAPI` (`Launch`/`Terminate`) + `StripeAZ`/`AZsForTier` seam the FSM
already depends on. The FSM, `MintShard`, the volume lifecycle, and the
heartbeat/bootstrap path are unchanged.

- **`Launch(spec)` — find-or-create by tag (idempotent / crash-safe):**
  1. `DescribeInstances` filtered by `tag:shard_id == spec.ShardID`, state
     `pending|running`. If one exists, adopt it (return its id) — a retry or
     crash-replay reuses the in-flight instance instead of minting a twin. The
     state filter excludes a just-reaped instance, so a recovery relaunch (after
     the dead one is terminated) correctly starts fresh.
  2. Else `RunInstances`: the tier's launch template + the AZ's subnet,
     `MinCount=MaxCount=1`, `TagSpecifications` stamping `shard_id` **at birth**
     (never momentarily untagged). No `ClientToken` — a stable token would,
     across recovery generations, return the prior (terminated) instance.
  3. Wait for `running` (an instance-state poll — replaces the ASG `InService`
     poll), then create+attach a fresh gp3 volume, or reattach
     `spec.ExistingVolumeID` (recovery). Volume reuse is also tag-idempotent via
     `VolumeManager.FindByShard`.
- **`Terminate(id)`** → `ec2:TerminateInstances`; already-gone is success.
- **Striping** is unchanged: `StripeAZ(tier, index)` picks the AZ; the
  provisioner maps AZ→subnet internally, so `MintShard`'s `spec.AZ` is untouched.

Data volumes survive instance termination (separately-created gp3s attached
post-launch, never in the instance block-device mapping, never
`DeleteOnTermination`), so recovery reattaches by tag exactly as before.

## Config (from SSM `config/app`)

- `SHARD_<TIER>_SUBNETS` = `"<az>=<subnet>,<az>=<subnet>"` (replaces
  `SHARD_<TIER>_ASGS`). Parsed by `parseShardSubnets`.
- `SHARD_<TIER>_LAUNCH_TEMPLATE` = the tier's launch-template id. Parsed by
  `parseShardTemplates`.

`startProd` selects `EC2Provisioner` when `SHARD_<TIER>_SUBNETS` is present.

## Infra (infrastructure-catalog, weft stack)

- A small `shard-fleet` module per tier creates a standalone `aws_launch_template`
  + `aws_security_group` (reusing the shard user-data; instances tagged
  `Role=shard-<tier>`). The 6 per-(tier,AZ) `asg-application` shard modules are
  removed.
- Coordinator IAM gains `ec2:RunInstances`, `ec2:TerminateInstances`,
  `ec2:CreateTags`, and `iam:PassRole` (scoped to the instance role+profile) and
  drops `autoscaling:SetDesiredCapacity` / `TerminateInstanceInAutoScalingGroup`
  / `DescribeAutoScalingGroups`. The `shard_volumes` policy is unchanged.
- Coordinator, compute, and loom-producer ASGs are untouched.

## Tooling

- `weft-admin roll-*` is launcher-agnostic (drives the coordinator's
  `RelaunchShard` gRPC, which runs the FSM recovery reap+relaunch) — unchanged.
- kevcons resolves shard roles by EC2 tag (`Role=shard-<tier>`) instead of ASG
  membership for `instance get`/`health`; `instance delete` on a shard
  terminates the instance (the coordinator relaunches); `asg-activities` /
  `refresh-status` / `status` degrade for shard roles. Shard roles in
  `.kevcons.yaml` are now per-tier (`shard-small`).

## Cutover runbook (prod, account 220664822695)

1. Deploy weft with `EC2Provisioner` **config-gated off** (no `SHARD_*_SUBNETS`
   yet) + the infra change (launch templates, subnet/template SSM params, IAM
   **add**). ASGs still present.
2. Flip config (`SHARD_<TIER>_SUBNETS` / `_LAUNCH_TEMPLATE`) → restart the
   coordinator → new shards launch as bare EC2; existing ASG shards keep running
   (tracked by `instance_id`).
3. **Immediately** `aws autoscaling suspend-processes` on the shard ASGs (no
   refill/replace) — also stops any further ghost creation.
4. `weft-admin roll-all` → every shard relaunches via `EC2Provisioner` as bare
   EC2 (reattaching its volume); old ASG instances terminated, ASG won't refill.
5. Terminate any leftover ASG instances (the existing ghosts die here); delete
   the shard ASG modules + autoscaling IAM + `SHARD_*_ASGS` config.
6. Ship the kevcons tag-visibility so shards stay observable.

## Verification

- Unit: `go test -tags test ./logdb/` — `EC2Provisioner` tests (fresh launch,
  idempotent replay → same id, recovery reattach, idempotent terminate,
  wait-for-running) + the unchanged FSM/coordination/integration/steady-state
  suites green against the new launcher.
- LocalStack: `integration/lifecycle_test.go` (build tag `integration,localstack`)
  drives real `RunInstances`/attach.
- Prod post-flip: `describe-instances tag:shard_id` shows new shards as bare
  instances; after `roll-all`, every `shard_records.instance_id` is a bare
  instance and the ghost invariant (no untagged shard instance, `desired==running`
  ratchet) can't recur — there is no ASG to refill.
