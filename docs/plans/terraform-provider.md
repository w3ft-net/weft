# Terraform provider for weft (design sketch)

A customer who is unhappy with the version history weft provides (the wit/git
surface) may want to keep their crons, alerts and dashboards in their own repo
and apply them with Terraform. This doc captures what a
`terraform-provider-weft` would involve. Nothing is built yet.

## Starting point: the REST API already covers most of it

From `api/handlers.go` and `api/dashboards.go`:

| Resource | Endpoints | Body |
|---|---|---|
| cron / alert | `GET/PUT/DELETE /api/crons/{name}`, `GET /api/crons` | `{schedule, pipeline}` |
| dashboard | `GET/PUT/DELETE /api/dashboards/{name}`, `GET /api/dashboards` | `{yaml_body}` |
| namespace | `/api/namespaces`, `PUT .../permissions/{userId}` | n/a |
| token | `/api/tokens` | n/a |

- An alert is a cron whose pipeline ends in `| alert <cond>`. `weft_cron` and
  `weft_alert` can be one resource; a separate `weft_alert` is only sugar.
- The PUT handlers write through the same `shellFS` mount the shell uses. Plan
  limits, schedule validation, the namespace read-permission check
  (`logreads`) and dashboard cron-reference validation all apply to
  Terraform for free. The provider cannot bypass them.

## Provider work

1. **Scaffolding.** Go, `terraform-plugin-framework` (not SDKv2). New repo or
   a `terraform-provider-weft/` directory. Registry publishing requires the
   `terraform-provider-<name>` repo name, GoReleaser and GPG-signed releases.
2. **Client.** A small Go client for the endpoints above. Keep its HTTP code
   separate from `weft-go` (the public SDK).
3. **Provider config.** `endpoint`, `token`, `org`, with `WEFT_ENDPOINT`,
   `WEFT_TOKEN`, `WEFT_ORG` env fallbacks (same variables the CLI uses).
4. **Resources.**
   - `weft_cron`: `name`, `schedule`, `pipeline`. Name change forces replacement.
   - `weft_dashboard`: `name`, `yaml_body`. Start with a raw YAML string
     (`file()` friendly); a structured block schema can come later.
   - `weft_namespace`, `weft_namespace_permission`: optional.
5. **Data sources.** `weft_cron`, `weft_dashboard`. Cheap once the client exists.
6. **Import.** By name via `GET /api/crons/{name}`; this is how existing
   crons get adopted without recreation.
7. **Acceptance tests.** `terraform-plugin-testing` against the local dev
   stack, authenticated with a minted JWT.

## Hard parts

- **Machine credentials.** A human JWT is a poor CI credential. Check whether
  `/api/tokens` can mint a long-lived, org-scoped (ideally write-scoped)
  service token. If not, building that is the first prerequisite. Token
  scopes have not been verified.
- **Drift and normalization.** `handleSaveWatch` prepends `# schedule: ...\n`
  to the pipeline, and save-time lowering may rewrite stored content. If GET
  after PUT does not return what was PUT, Terraform shows a permanent diff.
  Either the read path round-trips exactly or the provider normalizes.
- **Save warnings.** A successful save can carry `warnings` (`store.Warning`,
  e.g. the truncation lint). Surface them as Terraform diagnostics, not errors.
- **No concurrency control.** PUT has no ETag or version, so last write wins
  against the web UI and the `weft_*_propose` flow. Acceptable if documented as
  "Terraform owns these names"; an optional `If-Match` could come later.
- **Dashboard to cron dependency.** The `/etc` mount validates cron references
  on save, so a fresh create fails unless the crons exist first. Interpolating
  `weft_cron.x.name` into the YAML gives Terraform the implicit dependency.
- **Scripts.** Crons can call scripts. No REST route for scripts was found
  (they appear to live on the shell filesystem). A `weft_script` resource
  needs a new endpoint first.
- **Overlap with wit.** If a resource is Terraform-managed and someone edits
  it in the shell or UI, the next apply reverts it. Consider marking
  Terraform-managed resources read-only (or at least tagged) in the UI.

## Sizing

- Cron and dashboard resources, data sources, import and tests: a few days of
  focused work, assuming the round trip is clean.
- Machine tokens, a scripts endpoint and managed-by markers: server-side work
  that is probably larger than the provider itself.

## Open questions to settle first

1. Does `/api/tokens` support a service-account token today, and with what scope?
2. Does GET-after-PUT round-trip byte-for-byte for crons and dashboards? A
   failing round-trip test is the natural first piece of provider scaffolding.
3. Do we want a managed-by marker so the UI can warn about Terraform-owned resources?
