# GitHub Actions CI/CD with OIDC

## Overview

PR-based workflow: push branches, open PRs, merge to main triggers deploy. No long-lived AWS credentials — GitHub Actions authenticates via OIDC.

## Architecture

### OIDC Role (Terraform)

Uses the `github-asg-deployment` module from infrastructure-catalog. Lives in `infrastructure-live/prod/weft/github-deployment.tf`.

```hcl
module "github_deploy" {
  source = "../../../infrastructure-catalog/modules/github-asg-deployment"

  app_name          = "weft"
  github_repository = "graybeardcorp/weft"
  role_name         = "weft-github-deploy"
  create_oidc_provider = false  # verify if already exists in 220664822695

  tags = var.default_tags
}
```

Grants: Packer AMI builds, SSM parameter access (AMI IDs), ASG instance refresh, launch template management. No S3/CloudFront needed — weft has no separate static assets.

### GitHub Actions Workflows

#### `ci.yml` — runs on PRs and push to main

Four parallel jobs, partitioned by the docker overlay each one needs.
The harness no longer manages Docker itself — every job brings the
stack up explicitly before invoking `go test`, and `requireStackRunning()`
in `integration/harness.go` fails fast if nothing is listening on
`:9021/health`.

- **`unit`**: `go test ./...` (no Docker). The `integration` build tag keeps integration tests out.
- **`integration`** (generic): `docker compose up`, then `go test -tags integration`. Runs every test tagged `integration && !localstack` — the bulk of the suite (ingest/query, onboard, buckets, jq, lambda-cold-shard, direct-to-compute ops, ingest-auth).
- **`integration-localstack`**: `docker compose -f docker-compose.yml -f docker-compose.localstack.yml up`, then `go test -tags integration,localstack`. Runs only AWS-emulator-touching tests (`integration && localstack` files: lifecycle, prewarm, terminate-on-archive). Cleanly partitioned from `integration` via the `!localstack` constraint on the generic files.
- **`integration-coverage`**: `docker compose -f docker-compose.yml -f docker-compose.localstack.yml -f docker-compose.cover.yml up` to bring up coverage-instrumented binaries (per `docker-compose.cover.yml`). Runs both partitions back-to-back (`-tags integration` then `-tags integration,localstack`) so the merged profile reflects the entire surface, then `scripts/coverage-report.sh` consolidates per-service `.out` files and they're uploaded as a workflow artifact.

The build-tag convention:

| File set                                    | Constraint                              |
|---------------------------------------------|------------------------------------------|
| Generic integration tests                   | `//go:build integration && !localstack` |
| AWS-emulator-touching tests                 | `//go:build integration && localstack`  |
| Helpers (`auth_helpers.go` et al.)          | `//go:build integration` (visible to both) |
| TestMain owner (`cold_storage_test.go`)     | `//go:build integration` (visible to both) |
| Package-level helpers (`harness.go`, `coverage.go`) | `//go:build integration` (visible to both) |

Adding a new test that needs LocalStack: tag the file
`//go:build integration && localstack`. Adding a new test that doesn't:
`//go:build integration && !localstack`. The CI partitioning is enforced
at compile time.

All jobs need the `warp` repo checked out as a sibling (local replace
directive in go.mod: `replace github.com/kevlarcon/warp => ../warp`).
The workflow uses `actions/checkout@v4` with `path: weft` and a second
`path: warp` to lay them out next to each other. `graybeardcorp/warp`
is private, so the second checkout authenticates via a read-only deploy
key on the warp repo, with the private half stored as the
`WARP_DEPLOY_KEY` secret on weft. This avoids tying CI auth to any
individual's PAT.

#### `deploy.yml` — runs on push to main, after CI passes

1. Assumes the OIDC role via `aws-actions/configure-aws-credentials`.
2. Installs the kevcons binary via the composite action `graybeardcorp/kevcons/.github/actions/install` (downloads from `s3://kevlar-releases-289202785125/kevcons/`). No build step.
3. Runs `kevcons build -y`.
4. Runs `kevcons deploy -y`.

### Prerequisite: kevcons releases + base AMI integration

`deploy.yml` depends on the kevcons release bucket existing and the
`graybeardcorp/kevcons` repo having at least one `v*` tag pushed. See
`docs/kevcons_in_ami.md` for the AMI-side counterpart (Phase 3 of this
work) and `infrastructure-live/management/kevcons-releases/` for the
bucket + OIDC role (Phase 1).

### GitHub Repository Secrets

Two secrets on `graybeardcorp/weft`:

```
AWS_DEPLOY_ROLE = <role ARN from terraform output>
WARP_DEPLOY_KEY = <ed25519 private key; public half is a read-only deploy key on graybeardcorp/warp>
```

## Open Questions

- **warp dependency**: `graybeardcorp/warp` is referenced via local replace. CI fetches it via `actions/checkout` + a read-only deploy key (`WARP_DEPLOY_KEY` on weft, public half on warp). A cleaner long-term alternative would be to publish warp as a tagged Go module and drop the replace directive entirely; deferred until that friction shows up.
- **OIDC provider**: Confirm whether the GitHub OIDC provider already exists in account 220664822695 (grantcentral may have created it). If not, set `create_oidc_provider = true`.
- **kevcons in CI**: The deploy job needs `kevcons` installed. Options: build from source in the workflow, or publish a release binary.
- **api migration race on cold start**: When the stack comes up fresh, both `api` and `coordinator` race to apply goose migrations and the loser crashes on `relation already exists`. The CI workflow papers over this with a redundant `docker compose up -d` after the initial up, which restarts crashed services on Docker's healthcheck-aware retry path. A real fix would be to designate one service as the migration owner; tracked separately.
