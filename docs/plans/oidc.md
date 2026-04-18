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
  github_repository = "kevlarcon/weft"
  role_name         = "weft-github-deploy"
  create_oidc_provider = false  # verify if already exists in 220664822695

  tags = var.default_tags
}
```

Grants: Packer AMI builds, SSM parameter access (AMI IDs), ASG instance refresh, launch template management. No S3/CloudFront needed — weft has no separate static assets.

### GitHub Actions Workflows

#### `ci.yml` — runs on PRs and push to main

Two parallel jobs:

- **Unit tests**: `go test ./...` (fast, no Docker). The `integration` build tag keeps integration tests out by default.
- **Integration tests**: `go test -tags=integration ./integration/...`. The harness no longer manages Docker itself — the job brings the stack up explicitly (`docker compose up -d --build`) before running tests, and the harness fails fast with a clear message if nothing's listening on `:9021/health`. See `integration/harness.go`.

Both jobs need the `warp` repo checked out as a sibling (local replace directive in go.mod: `replace github.com/kevlarcon/warp => ../warp`).

#### `deploy.yml` — runs on push to main, after CI passes

1. Assumes the OIDC role via `aws-actions/configure-aws-credentials`.
2. Installs the kevcons binary via the composite action `kevlarcon/kevcons/.github/actions/install` (downloads from `s3://kevlar-releases-289202785125/kevcons/`). No build step.
3. Runs `kevcons build -y`.
4. Runs `kevcons deploy -y`.

### Prerequisite: kevcons releases + base AMI integration

`deploy.yml` depends on the kevcons release bucket existing and the
`kevlarcon/kevcons` repo having at least one `v*` tag pushed. See
`docs/kevcons_in_ami.md` for the AMI-side counterpart (Phase 3 of this
work) and `infrastructure-live/management/kevcons-releases/` for the
bucket + OIDC role (Phase 1).

### GitHub Repository Secret

After applying the Terraform, set one secret on `kevlarcon/weft`:

```
AWS_DEPLOY_ROLE = <role ARN from terraform output>
```

## Open Questions

- **warp dependency**: `kevlarcon/warp` is referenced via local replace. CI needs it checked out as a sibling. Should it stay as a local replace, or move to a versioned Go module reference?
- **OIDC provider**: Confirm whether the GitHub OIDC provider already exists in account 220664822695 (grantcentral may have created it). If not, set `create_oidc_provider = true`.
- **kevcons in CI**: The deploy job needs `kevcons` installed. Options: build from source in the workflow, or publish a release binary.
