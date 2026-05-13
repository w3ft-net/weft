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

### 2. Token storage in the kevcons-managed `/weft/*` namespace

Two secrets, flat names under `/weft/` in the existing
kevcons-managed Secrets Manager namespace:

| Secret name              | Value                         |
|--------------------------|-------------------------------|
| `/weft/LOOM_PROD_TOKEN`  | raw `wft_...` for `loom-prod` |
| `/weft/LOOM_SAND_TOKEN`  | raw `wft_...` for `loom-sand` |

SCREAMING_SNAKE_CASE names are required by `load-app-env`'s
contract — it iterates every `/weft/*` secret name with `kevcons
secrets list` and emits each as `NAME='value'` into `/opt/app/.env`.
Slashes in the secret name would either be skipped or get munged
into invalid env var names; the flat form makes load-app-env emit
`$LOOM_PROD_TOKEN` and `$LOOM_SAND_TOKEN` directly.

Terraform does **not** own these secrets. They're created and
maintained out-of-band via `kevcons secrets set` — same lifecycle
as every other `/weft/*` secret (`JWT_SECRET`, `WEFTD_TOKEN`, the
Stripe keys). The producer's IAM doesn't even need to be scoped
to them: the existing `weft-instance-profile` already grants
`secretsmanager:GetSecretValue` on `/weft/*`, which load-app-env
uses on every weft host. The two tokens become two more entries
in `/opt/app/.env`.

Token minting is manual: the operator runs `weft-demo-seed
--prod-loom --tokens-out=-` once against the prod DB, pipes the
two output values into `kevcons secrets set`, and never thinks
about them again. The seed binary is idempotent — it only mints
when no live token with the matching label exists, so re-running
is safe.

### 3. Minimal Terraform stack `prod/weft-loom-producer`

A standalone stack — the producer is independent of
coordinator/shard/compute scaling and shouldn't ride those AMI
rebuilds. Resource list is intentionally tiny because everything
about secrets, IAM, and config flow is already baked into the AMI
and the kevcons-managed `/weft/*` namespace.

`infrastructure-live/prod/weft-loom-producer/main.tf`:

- `data.aws_ssm_parameter.ami_id` reading `/weft/ami/latest` (the
  same parameter `asg-weft-application` reads).
- `data.aws_iam_instance_profile.weft` referencing the existing
  `weft-instance-profile`. The producer attaches it directly —
  it already grants the secretsmanager and SSM permissions
  load-app-env needs.
- `data.aws_subnets.weft_private` filtered by the asg-weft-application
  stack's `Tier=private` + `Name=weft-private-*` tags.
- One `aws_security_group` — egress-only, no ingress (operator
  access is via SSM Session Manager).
- One `aws_instance` — t4g.nano, `Role=loom-producer` tag,
  no public IP, IMDSv2 required, `instance_metadata_tags=enabled`
  so `startup.sh` can read the Role tag, gp3 root volume.

No user_data, no IAM resources, no aws_secretsmanager_secret
resources. The AMI does all the work via the existing
`weft-startup.service` → `startup.sh` → `load-app-env` →
`systemctl start weft-loom-producer.service` chain.

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

Three files in `deploy/loom-producer/`, all baked into the AMI by
`packer/weft.pkr.hcl`. Other roles ignore them entirely; only the
`Role=loom-producer` host activates them.

- `weft-loom-producer.service` — role unit. Named with the
  `weft-$ROLE` prefix so `startup.sh`'s existing
  `systemctl start weft-$ROLE` line picks it up for free.
  `EnvironmentFile=-/opt/app/.env` (the leading `-` makes missing
  the file non-fatal — `weft-startup.service` writes it before us
  via the `After=` ordering). `Wants=weftd-loom@prod.service
  weftd-loom@sand.service` so starting the role cascades to the
  two forwarders.
- `weftd-loom@.service` — templated, one instance per env.
  `EnvironmentFile=/opt/app/.env` so the unit sees every
  `/weft/*` secret kevcons emitted. `ExecStartPre` runs
  `render-weftd-config.sh %i`, which picks `LOOM_${ENV^^}_TOKEN`
  out of the environment and writes
  `/etc/weftd-loom-${env}/config.yaml` listing the three matching
  log paths with their `app:` tags. `PartOf=weft-loom-producer.service`
  so stopping the role cascades down too.
- `streams.yaml` at `/etc/loom-producer/`. Static manifest of the
  six streams with shapes + error-ratios — same values as
  `docker-compose.demo.yml`'s loom-* services.

No install.sh. No `systemctl mask weftd.service`. The role-skip
for the ops-side weftd lives in `deploy/startup.sh`: a
one-line `[ "$ROLE" != "loom-producer" ]` guard in front of the
existing block that starts the ops weftd.

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

1. **Seed Loom in the prod DB**, opening a tunnel through kevcons:
   ```bash
   cd ~/dev/weft
   go build -o /tmp/weft-demo-seed ./cmd/weft-demo-seed
   kevcons db tunnel -- bash -c '
     DB_DSN="host=$PGHOST port=$PGPORT user=$PGUSER password=$PGPASSWORD dbname=$PGDATABASE sslmode=require" \
       /tmp/weft-demo-seed --prod-loom --tokens-out=-
   '
   ```
   stdout has two lines: `LOOM_PROD_TOKEN=wft_...` and
   `LOOM_SAND_TOKEN=wft_...`. Idempotent: re-running skips mint if
   tokens already exist.

2. **Store the tokens** in the kevcons-managed `/weft/*` namespace:
   ```bash
   kevcons secrets set LOOM_PROD_TOKEN '<paste prod value>'
   kevcons secrets set LOOM_SAND_TOKEN '<paste sand value>'
   ```

3. **Apply the producer stack**:
   ```bash
   cd ~/dev/infrastructure-live/prod/weft-loom-producer
   terraform init
   terraform apply
   ```
   Instance launches. `weft-startup.service` runs `load-app-env`
   which writes `LOOM_*_TOKEN` into `/opt/app/.env`, then
   `systemctl start weft-loom-producer.service` cascades through
   the two `weftd-loom@` instances. First boot is clean — no
   crash-loop, no taint+reapply dance.

4. **Verify**. From any weft shell logged in as a real prod user
   with Loom membership (signing up a new account at
   `https://w3ft.net` works — the membership grant fires on
   signup):
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
