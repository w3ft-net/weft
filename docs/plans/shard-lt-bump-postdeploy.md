# Shard launch-template AMI bump via declarative post_deploy

## Problem

The shard fleet was refactored from ASGs to coordinator-managed direct EC2
launches: `logdb/provisioner_ec2.go:runInstance` calls `RunInstances` against
per-tier launch templates (`SHARD_*_LAUNCH_TEMPLATE`) with `Version: "$Latest"`.
The shard LTs carry `lifecycle { ignore_changes = [image_id] }` in Terraform —
"AMI managed by kevcons deploy, not TF".

But kevcons's deploy only bumps a launch template's AMI by discovering it
*through its ASG*. With the shard ASGs gone, **nothing bumps the shard LTs**, so
they froze on a now-deregistered AMI and `RunInstances` fails
`InvalidAMIID.NotFound`. The hardcoded `runWeftAdminRoll` in deploy.go still
rolls the fleet, but it rolls onto the stale template — and it runs *before* the
user `post_deploy:` hooks, so there's no slot to bump the LT first.

## Design

Make the deploy tail fully declarative and move the shard-fleet upgrade out of
hardcoded Go into YAML steps. Two reinforcing pieces:

1. **Generalize `post_deploy:` with `run_local` / `run_remote`.**
   - `run_local` — `sh -c` in AppRoot (current `run:` semantics), PLUS the
     deploy's assumed-role AWS creds injected into the step env (so a bare `aws`
     resolves to the weft account). `run:` stays as an alias for `run_local`.
   - `run_remote` — runs the command on the `entrypoint_role` (coordinator)
     instance via SSM SendCommand, reusing the 2h-streaming `runSSMCommand`
     machinery. Runs under the instance's own IAM role.
   - Validation: exactly one of `run` / `run_local` / `run_remote`. `commit:` is
     incompatible with `run_remote`. Step-level `on_error: warn|fail`.

2. **Self-describing `roll-all` (removes `--target-sha` threading).**
   Tag the AMI at build time with `ShardGitSHA = var.shard_git_sha` (the var
   already exists and is populated). `roll-all`, on the coordinator, derives the
   target SHA from the shard LT `$Latest` → `ImageId` → `ShardGitSHA` tag and
   rolls only stale shards. Empty tag → full roll (bootstrap-safe). This is what
   makes the two YAML steps independently idempotent: bump-without-roll just
   points the LT at a live AMI (organic launches pick it up), roll-without-bump
   is a no-op.

### weft `.kevcons.yaml` tail

```yaml
post_deploy:
  - name: Point shard launch templates at the new AMI
    run_remote: sudo /opt/app/bin/weft-admin-run bump-shard-templates
    on_error: fail
  - name: Roll the shard fleet onto the new AMI
    run_remote: sudo /opt/app/bin/weft-admin-run roll-all
    on_error: fail
  - name: Collapse migrations through the just-deployed AMI
    run_local: make collapse
    on_error: warn
    commit: { paths: [store/schema.sql, store/migrations/], message: "...", on_error: warn }
```

Also drop `shard-small` from `additional_roles` (its ASG is gone; it only
produces a "extra ASG not found, skipping" warning now) and the `shard_roles`
key entirely.

## Changes by repo

### kevcons
- `pkg/appconfig/yaml.go` — `PostDeployStep` gains `RunLocal`, `RunRemote`,
  `OnError`; add validation + `EffectiveLocalCommand()` helper. Remove
  `ShardRoles`, `IsShardRole()`, `HasShardRoles()`.
- `internal/postdeploy/runner.go` — `Runner` gains `AWSCfg *aws.Config` and
  `AppCfg *appconfig.KevConsConfig`; thread `ctx` through `Run`/`runStep`;
  inject creds (`awsutil.CredentialEnv`) for `run_local`; add `runRemote` (SSM);
  `handleError` reads step-level `OnError`.
- `pkg/deploy/deploy.go` — delete `runWeftAdminRoll`, the deploy-side
  `shardSubtreeSHA` dupe, the `HasShardRoles` gate, and the `IsShardRole` skip in
  the extra-ASG loop; extract `findHealthyCoordinatorInstance`; wire the runner
  with `AWSCfg`/`AppCfg`/`ctx`.

### weft
- `cmd/weft-admin/bump_shard_templates.go` (new) — read `/<app>/ami/latest` from
  SSM + `SHARD_*_LAUNCH_TEMPLATE` from env; `CreateLaunchTemplateVersion` per
  tier with the new ImageId.
- `cmd/weft-admin/main.go` — wire `bump-shard-templates`; `roll-all` derives
  target SHA from the LT/AMI tag when `--target-sha` is unset.
- `packer/weft.pkr.hcl` — add `ShardGitSHA = var.shard_git_sha` to AMI tags.
- `.kevcons.yaml` — the post_deploy rewrite above.

### infrastructure-catalog
- `stacks/asg-weft-application/main.tf` — add `ec2:CreateLaunchTemplateVersion`,
  `ec2:DescribeLaunchTemplateVersions`, `ec2:DescribeImages` to
  `aws_iam_role_policy.shard_ec2_launch`.

## Rollout ordering (critical)

1. Land + **`terraform apply` the IAM change first** — the coordinator role
   lacks `CreateLaunchTemplateVersion`, so `bump-shard-templates` would fail
   `AccessDenied` otherwise.
2. Merge the code (kevcons + weft).
3. Deploy. The new AMI (built this deploy, carrying the new `weft-admin` +
   `ShardGitSHA` tag) lands on the coordinator via its normal ASG refresh
   *before* post_deploy runs, so `bump-shard-templates` exists when first
   invoked — no chicken-and-egg beyond the IAM perm.

## Immediate prod unblock (already applied out-of-band)

`create-launch-template-version` on each shard LT pointing at `/weft/ami/latest`,
then re-roll. Restores the live fleet ahead of this permanent fix.
