# Plan: Loom producer host in prod

Stand up a single small EC2 instance in the weft account that runs
the same Loom Inc log generators we ship in dev's docker-compose
overlay, streaming through `weftd` to the live coordinator at
`ingest.w3ft.net:9020`. Pair it with a one-time seed of the Loom
customer (and its preseeded `/bin`, `/cron.d`, `notify.yaml`,
dashboards, error-rate history) into the prod database.

## Why

Onboarding shipped. Every signup now lands with a read-only
membership in Loom Inc and is funneled into a tour that walks
through Loom's data: `tail /log/loom/prod/api.log`, `chart
error-rate`, `weft.avg error-rate 1h`, the canned `wc -l | alert
above 0`, etc. The Loom customer doesn't exist in the prod DB yet,
and even once it does, the tour is dead air without a producer:
the `tail` lessons would show an empty stream, `error-rate` would
flatline at 0, and the `wit log` lessons would have nothing to
show. The dev compose overlay drives 6 generator containers
against the dev coordinator; we need the equivalent against the
prod coordinator.

The existing in-AMI `weftd` on coordinator/shard/compute hosts
reports kevlar-internal metrics under its own ops token — it is
not a producer for Loom, and conflating the two would mean every
coordinator restart blasts synthetic Loom traffic from a different
host_group. The producer needs to be its own host, with its own
identity, owned by the demo-data lifecycle and not the
coordinator's.

## Architecture

```
  ┌──────────────────────── weft-loom-producer (t4g.nano) ────────────────────────┐
  │                                                                                │
  │   ┌─────── loom-producer (single Go binary, 6 goroutines) ────────┐            │
  │   │                                                                │            │
  │   │  api-prod ─┐                       api-sand ─┐                 │            │
  │   │  worker-prod ─writes─► *-prod.log   worker-sand ─writes─► *-sand.log        │
  │   │  edge-prod   ─┘                     edge-sand   ─┘             │            │
  │   └────────────────────────────────────────────────────────────────┘            │
  │                                                                                │
  │   weftd-loom@prod.service   tails *-prod.log  ──┐                              │
  │                              (token: loom-prod)  ├──► ingest.w3ft.net:9020      │
  │   weftd-loom@sand.service   tails *-sand.log  ──┘   (NLB → coordinator)        │
  │                              (token: loom-sand)                                │
  │                                                                                │
  └────────────────────────────────────────────────────────────────────────────────┘
```

**One Go process drives all six streams.** Agent tokens bind to
`(customer, env)`, and a single `weftd` can tail multiple files
with distinct `app:` tags in one config — so we keep two `weftd`
instances (one per env, three log files each) but consolidate the
six per-service Python generators into one Go binary that emits
to six files concurrently. Net process count: 3 (1 generator + 2
weftd), down from 8 in the original Python sketch. Memory budget
drops from ~310 MB to ~60 MB, putting t4g.nano comfortably under
its 512 MB cap with room for the CloudWatch + SSM agents and OS.

Going through files (rather than weftd's local-ingest socket)
keeps the prod shape close enough to dev that the tailer-side
behavior — rotation handling, file-open recovery, multi-source
demuxing — is exercised by the same code path in both
environments.

## Components to build

### 1. Loom-prod seeder mode in `weft-demo-seed`

`cmd/weft-demo-seed/main.go` today is dev-only: it always reseeds
acme + globex + loom and **rotates** every token on every run.
Neither is acceptable in prod.

Add a `--prod-loom` flag with these semantics:

- **Only** seeds the Loom customer. acme + globex are skipped.
- Tokens are minted **only if no live token exists** for the
  label. Existing tokens are kept; no `RevokeAgentTokensByLabel`
  call. Re-running the binary is a no-op for tokens that already
  exist.
- Two tokens are minted instead of six: labels `loom-prod` (env
  `prod`) and `loom-sand` (env `sand`). The producer's
  `weftd` units forward all three apps for each env through the
  matching token.
- Preseeded `/bin`, `/cron.d`, `notify.yaml`, dashboards, and the
  ~70-minute error-rate history (`preseedLoomErrorRateHistory`) are
  all idempotent today and keep their existing semantics.
- The Loom users (`ops@loom.test`, `kai@loom.test`,
  `priya@loom.test`) are created as verified users so the
  preseeded `wit log` history attributes correctly. They keep
  `@loom.test` addresses; no real email is ever sent.
- Token output writes a `secrets.json` file (or stdout when
  `--tokens-out -`) with `{"loom_prod_token": "wft_...",
  "loom_sand_token": "wft_..."}` so a deploy operator can shove
  them straight into Secrets Manager.

Files: `cmd/weft-demo-seed/main.go` (add flag + branch), splitting
the shared seed steps from the dev-only customers if needed. Reuse
`seedLoomCustomer` / `preseedScripts` / `preseedLoomErrorRateHistory`
unchanged.

### 2. Token storage in Secrets Manager

One AWS Secrets Manager secret per environment, both in the weft
account (`us-east-2`):

| Secret name                          | Value (plain string)          |
|--------------------------------------|-------------------------------|
| `/weft/loom-producer/prod-token`     | raw `wft_...` for `loom-prod` |
| `/weft/loom-producer/sand-token`     | raw `wft_...` for `loom-sand` |

Two separate secrets (rather than a single JSON blob) so the
instance's IAM policy can grant `secretsmanager:GetSecretValue`
against the path prefix `/weft/loom-producer/*` without having to
re-issue when we add or rename a token. SSM `SecureString` is
equivalent here; Secrets Manager wins because the existing
`load-app-env` pattern in weft already pulls Secrets Manager
entries by convention and we get rotation-tracking metadata for
free.

Tokens are written **manually**: an operator runs `weft-demo-seed
--prod-loom --tokens-out=-` once against the prod DB, copies the
two values into Secrets Manager, and never thinks about it again.
Terraform must **not** manage the secret values (only the secret
resources + IAM); we don't want a `terraform apply` to silently
rotate producer tokens.

### 3. New Terraform stack `prod/weft-loom-producer`

A standalone stack (not part of `asg-weft-application`) — the
producer is independent of coordinator/shard/compute scaling and
shouldn't ride those AMI rebuilds.

`infrastructure-live/prod/weft-loom-producer/main.tf`:

- Single `aws_instance` (no ASG — there's no scaling story and no
  failover requirement; if it crashes, the tour goes quiet for a
  few minutes and the next signup sees a flat error-rate chart).
- `instance_type = "t4g.nano"` (with `t4g.micro` documented as the
  fallback if 512 MB ends up tight).
- AMI: **reuse the live weft AMI** (`local.ami_id` from the same
  SSM parameter `/weft/config/latest-ami`). The weft AMI already
  bundles `/usr/local/bin/weftd` and all the systemd plumbing for
  `load-app-env`. We pay a few extra hundred MB of disk for the
  coordinator/shard/compute binaries we won't run, in exchange for
  zero new AMI lifecycle. The instance ignores its Role tag (or
  carries `Role=loom-producer`, which `start-role.sh` no-ops on).
- IAM role with `secretsmanager:GetSecretValue` on
  `/weft/loom-producer/*` and the standard CloudWatch agent /
  SSM Session Manager permissions.
- Security group: egress only. Ingress is closed — operators reach
  it via SSM Session Manager (no SSH key, no public IP).
- Subnet: any private subnet with NAT egress (reuse one of the
  existing `nat_private_subnet_ids`).
- User data: writes `/etc/loom-producer.env` listing the six
  `(app, env, rate-shape, error-ratio)` tuples, fetches the two
  tokens from Secrets Manager, drops them into
  `/etc/default/weftd-loom-{prod,sand}`, and `systemctl enable
  --now` the eight units described below.
- Route53 entry: none. The host doesn't serve traffic.

Files:
- `infrastructure-live/prod/weft-loom-producer/{main,variables}.tf`
- (optional) `infrastructure-catalog/stacks/loom-producer/main.tf`
  if we want to lift it into a reusable module, but the resource
  set is small enough to live entirely in the live stack on first
  pass.

### 4. `cmd/loom-producer` — Go port of the generator

A new Go program at `cmd/loom-producer/main.go` (with shape
helpers in `cmd/loom-producer/shape.go`) that subsumes
`dev/demo/generator.py`. One process, six goroutines, six output
files. Reads its stream list from a tiny YAML or JSON config
baked into the AMI:

```yaml
# /etc/loom-producer/streams.yaml
streams:
  - app: api
    env: prod
    log: /var/log/loom/api-prod.log
    shape: "sine:25,50,600"
    error_ratio: "sine:0.04,0.10,900"   # matches dev compose's loom-api-prod
  - app: api
    env: sand
    log: /var/log/loom/api-sand.log
    shape: "const:6"
    error_ratio: "0.05"
  # …4 more
```

Port surface from the Python original:

- `RateShape` (const / sine / sawtooth / monotonic) — same arg
  grammar (`sine:base,amp,period` etc.). The
  `tokens_emitted_for_step` integration test in
  `dev/demo/test_generator.py` becomes a Go table test in
  `cmd/loom-producer/shape_test.go` covering identical inputs.
- `error_ratio` parser — same trick: a bare float means flat,
  `sine:lo,hi,period` means a sinusoidal sweep (this is what
  drives the loom-api-prod tour signal today; preserving the
  exact 4%→10%/900s sweep keeps the preseeded `cron_results`
  history continuous with live data).
- Per-app line content (the `line_for()` keyword pools) — port
  pool definitions to a Go map keyed by app name. Same words,
  same picks, same shape; tests against the Python emitter via a
  one-shot fixture comparison are not required since the
  coordinator side doesn't care about line content.
- File rotation: write opens with `O_APPEND|O_CREATE`, no
  rotation logic. Logrotate (or weftd's own size watch) handles
  the rest — same as the Python version, which doesn't rotate
  either.

Output is plain text lines (one per emit), identical wire shape
to today's Python output so the weftd tailer side stays unchanged.

Build path: `CGO_ENABLED=0 go build -o /opt/app/bin/loom-producer
./cmd/loom-producer`, added to `packer/weft.pkr.hcl`'s build step
alongside the existing weft binaries. Static, ARM64, ~10 MB on
disk. Python and the dev compose generator stay where they are —
dev keeps the one-container-per-service model.

### 5. Systemd unit set in the weft AMI

Add to the AMI (provisioned via `packer/weft.pkr.hcl`) — keeping
them in the same AMI means a `kevcons build` already ships them,
no second image to maintain:

- `deploy/loom-producer/loom-producer.service` — single unit, no
  templating. `ExecStart=/opt/app/bin/loom-producer --config
  /etc/loom-producer/streams.yaml`. `Restart=always`.
- `deploy/loom-producer/weftd-loom@.service` — templated unit, one
  instance per env. `EnvironmentFile=/etc/default/weftd-loom-${env}`
  (env carries `WEFTD_TOKEN`); `ExecStartPre` renders
  `/etc/weftd-loom-${env}/config.yaml` listing the three matching
  log paths with their `app:` tags.
- `deploy/loom-producer/install.sh` — idempotent installer called
  from user data: drops `streams.yaml`, enables
  `loom-producer.service` + `weftd-loom@prod.service` +
  `weftd-loom@sand.service`, creates `/var/log/loom`.
- `systemctl mask weftd.service` on this host so the AMI's
  ops-side weftd unit can't fire — this host has no
  coordinator/shard/compute role, and a stray ops-token weftd
  would just spam empty heartbeats.

### 6. Producer-specific weftd config rendering

The dev compose entrypoint at `dev/demo/entrypoint.sh` writes one
weftd config per container with a single source. The producer
needs the multi-source variant:

```yaml
# /etc/weftd-loom-prod/config.yaml
endpoint: ingest.w3ft.net:9020
token: ${WEFTD_TOKEN}
host_group: loom-producer
sources:
  - app: api
    path: /var/log/loom/api-prod.log
  - app: worker
    path: /var/log/loom/worker-prod.log
  - app: edge
    path: /var/log/loom/edge-prod.log
```

`host_group: loom-producer` makes it cheap to filter the host's
ops-side metrics (memory/cpu) out of the customer-visible streams
later if anyone notices. Token's `env` binding stamps `$ENV=prod`
on every row; the `app:` field per source stamps `$APP`. Verified
end-to-end shape: identical to what the dev producers send today.

## Run sheet

1. **Seed Loom in prod**. From a host with prod DB access (the
   bastion or a coordinator over SSM), run:
   ```bash
   weft-demo-seed --prod-loom --db-dsn "$PROD_DSN" --tokens-out -
   ```
   Capture the two tokens.
2. **Stash tokens in Secrets Manager**:
   ```bash
   kevcons aws secretsmanager put-secret-value \
     --secret-id /weft/loom-producer/prod-token --secret-string "$LOOM_PROD"
   kevcons aws secretsmanager put-secret-value \
     --secret-id /weft/loom-producer/sand-token --secret-string "$LOOM_SAND"
   ```
3. **Apply the producer stack**:
   ```bash
   cd ~/dev/infrastructure-live/prod/weft-loom-producer
   terraform apply
   ```
   Instance comes up, user data starts the eight units.
4. **Verify** from the api host (or any weft shell logged in as a
   real prod user with Loom membership):
   ```
   weft> tail /log/loom/prod/api.log
   weft> tail /log/loom/prod/api.log | grep ERROR | wc -l
   weft> chart 1h error-rate
   ```
   Logs flowing means the tour now works against live data instead
   of the static preseeded history.

## Estimated effort

| Component                                           | Effort |
|-----------------------------------------------------|-------:|
| `--prod-loom` flag + idempotent token mint          |  0.75d |
| Secrets Manager wiring (TF + load-app-env)          |  0.5d  |
| `prod/weft-loom-producer` stack                     |  1d    |
| `cmd/loom-producer` Go port (shapes + content)      |  1d    |
| AMI: producer unit + `weftd-loom@` units + installer|  0.75d |
| Producer-specific weftd multi-source config render  |  0.25d |
| End-to-end verification + run-sheet polish          |  0.5d  |
| **Total**                                           | ~4.75d |

## Non-goals

- **No autoscaling, no failover.** A single t4g.nano with `restart=always`
  systemd units is the entire HA story. If the host dies, the next
  onboarding tour shows a flat chart until someone notices and
  re-applies. This is acceptable demo infrastructure; promoting it
  to multi-instance is on the table only if outage MTBF turns out
  to be a real problem.
- **No multi-region.** Producer lives wherever the coordinator
  lives (`us-east-2`).
- **No protocol changes.** `weftd`'s existing multi-source config
  is reused as-is. We're not extending the agent.
- **No automatic token rotation.** Tokens are one-time mints,
  stable for the life of the customer. If a token has to be
  rotated, that's a manual rerun of step 1–2 of the run sheet.
- **No dev-environment changes.** docker-compose.demo.yml stays
  as the dev story; the prod producer is a separate code path and
  doesn't replace dev's 6-container model.
- **No dashboards or alerting** on the producer host itself
  beyond the AMI's existing ops-side weftd. If the producer goes
  silent it'll show up as flat traffic on Loom's streams, which
  the onboarding tour already exercises.

## Open questions

- **Do we want @loom.test users in the prod DB long-term?** They
  exist to make `wit log` show non-NULL authors, but they're
  unreachable email addresses. Alternative: synthesize the wit
  history with NULL authors and drop the fake users. Keeping the
  three fake users matches dev exactly and is the lower-risk
  default; revisit if anyone confuses them for real users.
- **Should the Go producer write directly to weftd's local-ingest
  socket** (`/var/run/weftd-loom-{env}.sock`) instead of through
  files? It would drop the disk-I/O step and save a tail loop per
  source. The reason this plan stays on files: it keeps the prod
  wire-shape identical to dev's docker-compose path (where weftd
  tails files in each container), so any rotation / open-recovery
  bug surfaces in both places. Worth revisiting if disk wear or
  log-volume growth ever becomes a real concern — neither is
  likely at 6 streams of ~50 lines/sec peak.
