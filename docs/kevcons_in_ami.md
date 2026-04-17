# kevcons in the AMI (Phase 3 of S3-backed release distribution)

## Goal

Every AMI baked by Packer has `/usr/local/bin/kevcons` on PATH so that:

- Runtime systemd units / instance scripts can call `kevcons config refresh`,
  `kevcons secrets ...`, etc., without needing ambient Go or a build step.
- Ops one-offs (`ssm start-session` into a box, run `kevcons instance ...`)
  Just Work.
- Instance boot doesn't depend on GitHub availability — the binary is on
  the root filesystem at bake time.

## Where this happens

One place: the shared Ansible role that runs during **base AMI builds**
(`infrastructure-catalog/packer/ansible/`, the role that already installs
`aws`/`ssm-agent`/`cloudwatch-agent`). Every app-specific AMI
(`weft.pkr.hcl`, `xylem`, `grantcentral`, etc.) is layered on top of a
base AMI, so the binary flows through automatically.

Do **not** install kevcons in individual app pkr.hcl files. If we bake it
per-app, different app AMIs drift to different kevcons versions for no
reason.

## What runs during the Ansible task

A single `shell` task that invokes the install script from the kevcons
repo. The script is in S3 too, versioned alongside the binaries — no
GitHub auth needed at AMI bake time, only AWS.

```yaml
# infrastructure-catalog/packer/ansible/roles/baseline/tasks/main.yml
- name: Install kevcons from releases bucket
  become: yes
  shell: |
    set -euo pipefail
    REGION="${AWS_REGION:-us-east-2}"
    BUCKET="kevlar-releases-289202785125"
    VERSION=$(aws s3 cp --region "$REGION" "s3://${BUCKET}/kevcons/LATEST" -)
    OS=$(uname -s | tr '[:upper:]' '[:lower:]')
    ARCH=$(uname -m)
    case "$ARCH" in
      aarch64|arm64) ARCH=arm64 ;;
      x86_64|amd64)  ARCH=amd64 ;;
    esac
    aws s3 cp --region "$REGION" --no-progress \
      "s3://${BUCKET}/kevcons/${VERSION}/kevcons-${OS}-${ARCH}" \
      /usr/local/bin/kevcons
    chmod +x /usr/local/bin/kevcons
    /usr/local/bin/kevcons --version
  args:
    creates: /usr/local/bin/kevcons
```

(Prefer calling `scripts/install-kevcons.sh` directly if we can ship the
kevcons repo onto the Packer instance, but that adds another clone. The
inline variant keeps dependencies to just `aws` and the bucket.)

## Credentials at bake time

The Packer instance for base AMI builds assumes a cross-account role into
whichever app account is being baked. Those instance-profile credentials
are what Ansible uses for `aws s3 cp` in the task above. No new IAM is
needed because the releases bucket's policy grants `s3:GetObject` +
`s3:ListBucket` on `kevcons/*` to any principal in the Kevlar AWS org
(via `aws:PrincipalOrgID`).

## Version handling

- Bake-time: the Ansible task resolves `LATEST` once and pins the binary
  into the AMI. An AMI built on 2026-04-18 against `LATEST=v0.2.0` will
  carry `kevcons v0.2.0` for its entire life, regardless of later
  releases.
- Runtime: if we later need to nudge a running fleet onto a newer
  kevcons without a full AMI rebake, `scripts/install-kevcons.sh` can be
  run ad-hoc via `ssm send-command`. Not expected to be common — AMI
  rebuilds are the primary channel.
- Pinning: if a base AMI ever needs to be locked to a specific kevcons
  version (e.g., because a newer release broke something), replace
  `VERSION=$(aws s3 cp … LATEST -)` with a literal `VERSION=v0.1.5` in
  the Ansible task. Revert when fixed.

The **Backwards Compatibility Policy** in `kevcons/CLAUDE.md` makes the
"newer release broke something" case rare by design — breaking changes
require explicit sign-off and a deprecation window.

## Testing the rollout

1. Merge the Ansible task into infrastructure-catalog. Tag a new
   `vX.Y.Z-base` (or whatever the base AMI tag scheme is).
2. `kevcons build-base` — produces a new base AMI. Verify `/usr/local/bin/kevcons`
   is present and `kevcons --version` reports the expected tag on a
   test-launched instance from that AMI.
3. `kevcons build` (for any app using the new base) — confirm the
   binary flows through the layered AMI.
4. Once confidence is high, roll base AMIs for all apps on their next
   regular bake cycle — no forced rollout.

## What this unblocks

- GitHub Actions deploy workflows can drop the "install kevcons" step
  entirely when running on self-hosted runners / a container image
  built from the same base AMI (probably not how we run Actions today,
  but the option exists).
- Runtime components of the platform can call `kevcons` directly
  instead of reimplementing its logic. Concrete near-term candidate:
  having the coordinator's shard provisioner call `kevcons instance
  terminate` through the binary rather than reimplementing the ASG
  termination API call.

## Open questions

- Should the install script live at `s3://…/kevcons/install.sh` so a
  one-liner `curl | aws s3 cp | bash` works for non-kevcons-repo
  consumers? Deferred — add when first needed.
- Should we publish a `kevlar-base` Docker image with the same setup so
  CI (and local dev) can use the identical binary+environment without
  spinning an EC2? Deferred — not the current pain point.
