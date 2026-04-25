# Plan: AWS source presets

Shipped schema templates for common AWS log sources — VPC Flow Logs, CloudTrail, ALB / NLB, S3 access, AWS WAF, Lambda, RDS, API Gateway — so a customer adding one of these goes from "author YAML from scratch" to "pick from the list." Reduces time-to-first-row from ~30 minutes to ~30 seconds.

## Why add this

Today the `weft source add` flow (docs/manual/cloudwatch.md) requires a hand-written schema per source: timestamp field, format, custom field extraction, optional `expand_keys`. For well-known AWS log shapes — where the structure is published, versioned, and identical across every AWS account — forcing each customer to rediscover the right schema is both annoying and error-prone. Nearly every schema bug we've seen in onboarding has been for a source whose shape is in AWS docs and we could have shipped as a preset.

This is lowest-effort, highest-polish work. A preset is a YAML file with a known answer; the customer picks the answer from a menu.

## Architecture

Presets ship as static YAML under `coordinator/presets/aws/`:

```
coordinator/presets/aws/
  vpc-flow-logs.yaml
  cloudtrail.yaml
  alb.yaml
  nlb.yaml
  s3-access.yaml
  waf.yaml
  lambda-default.yaml          # stdout/stderr from Lambda's auto-created log groups
  rds-mysql-error.yaml
  rds-postgres-log.yaml
  api-gateway-access.yaml
```

Each file is the same schema YAML a customer would write today, plus a short metadata header:

```yaml
preset:
  id: vpc-flow-logs
  label: VPC Flow Logs
  description: >
    AWS VPC Flow Logs in the default v2 format or extended v5 format.
    Auto-detects which version from the first record.
  log_group_pattern: "/aws/vpc/flowlogs/*"       # used to hint the preset in the UI
  fields_captured:
    - interface-id
    - src-addr
    - dst-addr
    - src-port
    - dst-port
    - protocol
    - action
  docs_url: https://docs.aws.amazon.com/vpc/latest/userguide/flow-logs.html

schema:
  app: vpc-flow
  format: text
  field_separator: " "
  timestamp_field: "11"          # "start" field in v2
  timestamp_format: "unix"
  level_field: "13"              # "log-status"
  expand_keys: []
```

Embedding the schema in the same file (rather than a sidecar) means the presets directory is literally grep-able and a single commit maintains them.

## Selection UX

### CLI

```
weft source add --preset vpc-flow-logs \
  --log-group /aws/vpc/flowlogs/primary \
  --account 123456789012 \
  --role-arn arn:aws:iam::123456789012:role/weft-ingest \
  --external-id ...
```

The preset resolves the `--schema` argument. Customer provides only the AWS-account-side config (role, account, log group).

### Web UI

The "Add CloudWatch source" form gains a dropdown at the top: "What kind of log is this?"

- Dropdown options are the preset labels.
- Picking one auto-fills the app name, collapses the schema-YAML textarea (hidden but editable for advanced users who want to override), and pulls the docs link into a help sidebar.
- A "Custom schema" option drops back to today's free-form YAML.

### Auto-hint from log-group name

When a customer types a log group name matching a preset's `log_group_pattern` (glob), the UI surfaces a banner: "This looks like VPC Flow Logs — use that preset?" Opt-in to reduce surprise; never auto-switches without a click.

## Components to build

1. **`coordinator/presets/`** — directory of preset YAML files, loaded on coordinator start into an in-memory map. Hot reload on SIGHUP (or just require a coordinator restart for v1, which is fine for a static config).
2. **`GET /api/presets`** — returns the catalog (ids, labels, descriptions, `log_group_pattern`s) for the UI to render.
3. **`weft source add --preset <id>`** — CLI resolves preset locally by fetching `/api/presets/{id}` and using the embedded schema for the source-registration RPC.
4. **Web UI source form** — dropdown + conditional render + auto-hint banner.
5. **Auto-detect edge cases** — for VPC Flow Logs specifically, v2 and v5 have different field counts; the schema's parser selects at first-record time. Worth a small helper in the v2/v5 preset that picks the right `timestamp_field` index.
6. **Docs** — update `docs/manual/cloudwatch.md` to lead with "If your source is one of these, skip to the preset list." New `docs/manual/aws-presets.md` with one subsection per preset showing the example line, detected fields, and a known-gotchas list ("CloudTrail lambda-request events have an empty `userIdentity.userName`").
7. **Tests** — for each preset, a fixture log line + an expected parsed row. Regression-proof against AWS quietly rotating a format.

## Maintenance posture

AWS does rev log formats (VPC Flow v2 → v5, ALB adding fields). Each preset carries a `format_version` in the metadata and a schema version in the embedded fields. Adding a new format version means adding a new preset or extending the existing one; old customers' sources keep their current schema until they update. No auto-migration.

## Estimated effort

- Preset loader + catalog API: **0.5 day**
- One preset file (including test fixture): **~0.5 day each** — ~5 days for a starter set of 10
- CLI flag + UI dropdown + hint banner: **1 day**
- Docs pages + per-preset sections: **1 day**
- Regression tests harness: **0.5 day**

**Total: ~8 days** to ship a good starter set of 10 presets, tested, documented, and UX-polished.

Caveat: effort scales linearly with preset count. The preset loader + UX is ~2.5 days; after that each preset is independent and parallelizable.

## Non-goals

- **No non-CloudWatch AWS sources in v1.** S3-hosted access logs arrive via a different ingestion path that we don't have yet. When that path lands, presets follow naturally.
- **No non-AWS presets.** Azure / GCP / Kubernetes are separate plans with separate ingestion stories.
- **No auto-enrollment of every log group in the account.** Preset picks are per-source. If a customer has 200 Lambda functions, they still add 200 sources (or one glob), just with the preset applied to each.
- **No ML-based format detection.** The `log_group_pattern` auto-hint uses plain globs. If the customer's log group doesn't match any pattern, they pick from the menu. No attempt to "sniff" the content of an unknown log group.
- **No version-upgrade automation.** When AWS extends VPC Flow to a new version, customers on the old preset keep the old schema until they explicitly pick the new preset. No silent reparses.
