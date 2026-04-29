# Plan: Multi-customer demo seed

A docker-compose-driven scenario that spins up two real weft customers, each with a 3-person team and two environments of three services apiece. acme has `sand` + `prod`; globex has `staging` + `prod`. The asymmetry is deliberate — it lets us exercise queries that span environments unique to one customer without leaking out the other side.

The point is to dogfood weft as a team — see what `ls -al` looks like with multiple authors, what `wit log` looks like after some real iteration, what cross-customer queries look like, what the new `chart` builtin shows when watch values follow a known shape — and to verify outside of the test suite that customer A literally cannot see customer B's data.

## Why

The unit + integration suites assert isolation in synthetic shapes. They prove the SQL filter is correct, but they don't tell us what it *feels* like to have two tenants live in the same coordinator at once. A persistent, reproducible demo environment closes that loop:

- Run two `weft` shells side by side, log in as alice@acme and dana@globex, and visually confirm that `cat *.log` returns disjoint sets.
- Have `ls -al /bin` show real authors (not all NULL, not all "alice"), so the attribution work pays out in a tangible way.
- Have `wit log /bin/scan` show an actual back-and-forth between teammates — the kind of history `wit checkout` was designed to recover.
- Hit edge cases that pure unit tests miss: token-bound `env` stamping, weftd reconnection storms when 12 agents come up at once, namespace permission wiring when the mid-tier user can read but not edit watches.

It's also a smoke test for the upcoming launch posture — before showing this to anyone, we should have used it ourselves with more than one fictional team.

## Scope

### Two customers

| Customer name | Display name   | Slug used in tokens / namespaces |
|---------------|----------------|----------------------------------|
| `acme`        | Acme Robotics  | `acme`                           |
| `globex`      | Globex Foods   | `globex`                         |

Both seeded via `FindOrCreateCustomer` + `UpdateCustomerDisplayName`.

### Six users

3 users per customer. The role spread is meaningful — it gives the namespace-permissions surface something real to do.

| Email                    | Customer | Display name   | Role posture                                         |
|--------------------------|----------|----------------|------------------------------------------------------|
| alice@acme.test          | acme     | Alice Chen     | full access on every acme namespace; can_create_namespace=true |
| bob@acme.test            | acme     | Bob Patel      | full access on every acme namespace                  |
| carol@acme.test          | acme     | Carol Reyes    | read-only on every acme namespace                    |
| dana@globex.test         | globex   | Dana Okafor    | full access on every globex namespace; can_create_namespace=true |
| eli@globex.test          | globex   | Eli Bergstrom  | full access on every globex namespace                |
| frida@globex.test        | globex   | Frida Lukoja   | read-only on every globex namespace                  |

Created with `CreateVerifiedUser` (skip the magic-link round-trip for the seed). Display name set with the existing `UpdateUserName` path. Permissions seeded with `GrantFullNamespaceAccess` for the two writers per team and a single-row insert for the read-only third.

### Two environments per customer (asymmetric)

| Customer | Envs              |
|----------|-------------------|
| acme     | `sand`, `prod`    |
| globex   | `staging`, `prod` |

These map to the `agent_tokens.env` column added in migration 034 — every token minted for the seed binds one of these values, and every `LogRow` ingested through that token gets that `env` stamped at the coordinator. Both customers share `prod` as a name; they're isolated by `customer_id`, not by env-name uniqueness, so this is a useful test of the multi-tenancy story.

### Three services per environment

Twelve services in total (2 customers × 2 envs × 3 services):

| Customer | Env     | Services (namespace/app)                            |
|----------|---------|-----------------------------------------------------|
| acme     | sand    | acme/api, acme/worker, acme/edge                    |
| acme     | prod    | acme/api, acme/worker, acme/edge                    |
| globex   | staging | globex/orders, globex/billing, globex/notifier      |
| globex   | prod    | globex/orders, globex/billing, globex/notifier      |

Note that each app exists in both of its customer's envs — that's the realistic shape (one app, two deployments). Distinguished on shard rows by the `env` column. Each customer has a single namespace (`acme`, `globex`) and three apps within it; this matches the `namespace/app` convention weftd uses.

### Twelve docker containers

Each service is one container:

- Image: a tiny one we build once, called `weft-demo-service`. Bundles `weftd` + a small Python text generator + a tini-style PID 1 wrapper.
- Entry script per container takes `SERVICE`, `ENV`, `CUSTOMER`, plus a rate-shape spec (see below) and `ERROR_RATIO` from env vars, runs the generator into `/var/log/<service>.log`, and runs `weftd` against that file with metrics enabled.
- The generator emits realistic-looking lines whose rate (lines/sec) follows a configured shape function. Format roughly:
  ```
  2026-04-25T10:00:00.123Z INFO  request method=GET path=/health status=200 dur=4ms user_id=u_4f
  2026-04-25T10:00:00.245Z WARN  cache_miss key=session:abc123
  2026-04-25T10:00:00.512Z ERROR upstream_timeout endpoint=auth-svc dur=2003ms
  ```
  Levels weighted ~80% INFO, 15% WARN, 5% ERROR by default. `ERROR_RATIO` per-service overrides this (a couple of services run "noisy" so the demo has alerting material). HTTP status / endpoint / user_id pulled from a small fixed pool so jq/awk grouping queries return interesting shapes.
- Per-service token + env stamped into the container at compose-up time. The token is consumed by weftd and never appears in the generator output (don't want to muddy "did the generator write this or did weftd?").
- **weftd metrics on by default.** Each rendered weftd config sets `metrics.enabled: true` with a 10s interval. That gives us real container-level cpu/memory/disk/load/network rows on `<namespace>/<deployment>_metrics` for every service, which the new `chart` builtin can plot directly. Combined with the rate-shape generator (below) we get two independent signals to chart: the synthetic "request rate" curve and the real container resource usage that the rate-shape pushes around as a side effect.

The compose file places all twelve containers on the existing weft network so they can hit `coordinator:9020`. They take their token from a per-service env var injected by the seed script.

### Rate-shape functions

Every service's `RATE_SHAPE` is one of:

| Shape | Params | Curve |
|-------|--------|-------|
| `const:R` | `R` lines/sec | Flat. Useful as a control. |
| `sine:base,amp,period` | base + amp · (1+sin(2π·t/period))/2 | Smooth oscillation between `base` and `base+amp` over `period` seconds. |
| `sawtooth:base,slope,reset` | climbs from `base` at `slope` lines/sec/sec, snaps back to `base` when rate ≥ `reset` | Periodic ramp. The reset threshold is the peak; `period = (reset−base)/slope`. |
| `monotonic:base,slope,reset?` | climbs from `base` at `slope` lines/sec/sec; resets to `base` only when rate ≥ `reset` if provided, otherwise grows unbounded | Indistinguishable from sawtooth when `reset` is set; bare-grow when omitted. |

`monotonic` without a reset is the "watch this leak forever" shape — useful for verifying that the new `chart` builtin's auto-fit redraws as the y-axis blows out. With a reset it overlaps with sawtooth on purpose, so we can place a `monotonic:0,0.1,30` next to a `sawtooth:0,0.1,30` and confirm they look the same — exposes any drift-vs-period-vs-jitter accidents in the renderer.

Sample assignment across the twelve services (rough plan, tunable in the seed CLI):

| Service                 | Shape                            | Why                                          |
|-------------------------|----------------------------------|----------------------------------------------|
| acme/api (sand)         | `const:8`                        | Quiet baseline — control case.               |
| acme/api (prod)         | `sine:20,40,600`                 | Diurnal-ish curve, biggest signal in the demo. |
| acme/worker (sand)      | `sawtooth:2,0.05,12`             | Slow climb + reset — easy to spot in chart.  |
| acme/worker (prod)      | `sine:5,10,300`                  | Smaller sine; charts overlay nicely with api/prod. |
| acme/edge (sand)        | `const:3`                        |                                              |
| acme/edge (prod)        | `sine:10,15,180`                 | Fast oscillation — tests chart's time bucketing. |
| globex/orders (staging) | `sawtooth:1,0.02,8`              |                                              |
| globex/orders (prod)    | `sine:30,60,900`                 |                                              |
| globex/billing (staging)| `monotonic:1,0.02`               | The unbounded leak case.                     |
| globex/billing (prod)   | `monotonic:5,0.05,40`            | Same shape, bounded — paired comparison.     |
| globex/notifier (staging)| `const:4`                       |                                              |
| globex/notifier (prod)  | `sine:8,16,420`                  |                                              |

Generator implementation: a single python loop with `time.monotonic()` driving the shape function; current-rate recomputed every 250ms; lines emitted by accumulating `rate*dt` until ≥ 1.0 then writing one line and decrementing. Cheap, keeps the timing honest within ~half a second.

## Pieces to build

### 1. Seed CLI (`cmd/weft-demo-seed/`)

A small Go binary that talks to the coordinator's database directly (DSN from env) and:

1. Idempotently creates the two customers + display names.
2. Idempotently creates six users (`CreateVerifiedUser`), sets names, sets `can_create_namespace` on the two leads.
3. Idempotently creates the two namespaces (`acme`, `globex`) — one per customer.
4. Grants permissions:
   - Two leads + middle: `GrantFullNamespaceAccess` on their team's namespace.
   - Read-only third: insert a `NamespacePermission{can_read=true}` row, all other booleans false.
5. Mints twelve agent tokens (one per service-env combination, label = `<service>-<env>` for traceability) with the right `env`. **Writes the raw tokens to `dev/demo/tokens.env`** — a single file the compose job reads. This is the only moment the raw tokens exist server-side; same as production, the DB just keeps the hash.
6. Idempotently creates twelve `log_groups` rows via `AddLogGroup`, one per (namespace, app), source_type=weftd, account=NULL. Schema YAML is the same minimal one we use in `dev/testapp.log` today.
7. Seeds a handful of `/bin/` scripts and `/cron.d/` watches per customer with realistic authorship + a couple of `wit` versions each (see "Pre-seeded content" below).

Re-running it is idempotent across the board: every step is `FirstOrCreate` or "skip if exists by hash". Token re-issue is gated by `tokens.env` already existing — if it does, the seed reuses what's there; if not, it mints fresh and writes a new file.

This is a separate binary, not a `seed` subcommand on `weft-migrate`, because (a) it depends on writing `tokens.env` to a host-visible path that the migrator never touches, and (b) we want the existing migrator to stay focused on schema.

### 2. Demo service image (`dev/demo/Dockerfile`)

```Dockerfile
FROM debian:bookworm-slim
COPY weftd /usr/local/bin/weftd
COPY entrypoint.sh /entrypoint.sh
COPY generator.py /generator.py
RUN chmod +x /entrypoint.sh /generator.py
ENTRYPOINT ["/entrypoint.sh"]
```

`entrypoint.sh`:
- Renders `/etc/weftd/config.yaml` from envvars (`SERVICE`, `CUSTOMER`, `ENV`, `WEFTD_TOKEN`). The rendered config sets `metrics.enabled: true, interval: 10s` so we get real container metrics flowing to `<namespace>/<deployment>_metrics` per migration 033.
- Starts the python generator in the background, writing to `/var/log/${SERVICE}.log`. Generator reads `RATE_SHAPE` and `ERROR_RATIO` env vars.
- `exec`s `weftd --config /etc/weftd/config.yaml`. weftd is PID 1, so `docker stop` cleanly tears down the agent and the SIGTERM cascades to the generator child.

`generator.py` — the shape engine. Parses `RATE_SHAPE` per the grammar above, runs the emit loop, picks line content from a small pool keyed by `SERVICE` (api emits `request method=...`; worker emits `job_completed job_id=...`; etc., so jq/awk queries against each app see app-shaped fields).

### 3. Compose overlay (`docker-compose.demo.yml`)

Extends the base `docker-compose.yml` (don't modify it). Defines the twelve service containers using yaml anchors to keep it short. Reads `dev/demo/tokens.env`. Example shape:

```yaml
services:
  acme-api-sand:
    extends:
      file: docker-compose.demo-base.yml
      service: demo-service
    environment:
      SERVICE: api
      CUSTOMER: acme
      ENV: sand
      WEFTD_TOKEN: ${ACME_API_SAND_TOKEN}
      RATE_SHAPE: "const:8"
      ERROR_RATIO: "0.05"
  acme-api-prod:
    extends: { file: docker-compose.demo-base.yml, service: demo-service }
    environment:
      SERVICE: api
      CUSTOMER: acme
      ENV: prod
      WEFTD_TOKEN: ${ACME_API_PROD_TOKEN}
      RATE_SHAPE: "sine:20,40,600"
      ERROR_RATIO: "0.03"
```

…times twelve, with the shapes from the table above. The `dev/demo/up.sh` wrapper is the friendly entry point — it runs the seed CLI, then `docker compose -f docker-compose.yml -f docker-compose.demo.yml --env-file dev/demo/tokens.env up -d`.

### 4. Pre-seeded `/bin/` and `/cron.d/` content

This is the part that makes `ls -al` and `wit log` interesting. For each customer:

- **`/bin/scan`** — three versions, written initially by user1, refined by user2, refined again by user1. So `wit log` shows the alternation.
- **`/bin/count_errors`** — single version by user2.
- **`/bin/mem_history`** — single version by user2. Reshape script for piping into `chart`: emits `{ts, pct}` JSONL from a metrics log so the user can do `mem_history /log/<...>/api_metrics.log | chart -x .ts -y .pct --bucket 1m -a avg`.
- **`/cron.d/error-rate`** — two versions: initial single-line by user1, expanded to use `weft.avg` aggregation by user2.
- **`/cron.d/mem-pressure`** — single version by user1. Per-host memory alert that exercises `alert -k .pct -t .host` against JSONL produced by `jq 'select(.type == "memory")'`. No `jq -r` reformatting; the alert reads JSON paths directly.

Authored by setting `weft.acting_user_id` GUC the same way live code does. The seed CLI does this inline. We also stamp a few of the version rows with commit messages so the `wit log` output isn't all blank.

### 5. `dev/demo/teardown.sh`

`docker compose -f docker-compose.yml -f docker-compose.demo.yml down -v` plus a `weft-demo-seed --reset` that truncates the demo customers (cascades through users, namespaces, log_groups, tokens, scripts, watches, history). Leaves anything *not* tagged demo alone — e.g. the existing `kevlar` dev customer keeps working.

## Verification checklist

Once seeded, the following should all hold without writing a single Go test. Each is a manual `weft` session.

### Cross-tenant isolation (the headline check)

In one terminal:
```
$ weft login alice@acme.test
weft> cd /log/acme/prod
weft> cat *.log | head -1
2026-04-25T10:01:33.421Z INFO request method=GET path=/orders ...   # acme/api
weft> ls /log/acme/prod
api.log  edge.log  worker.log  api_metrics.log  worker_metrics.log  edge_metrics.log
```

In another terminal at the same time:
```
$ weft login dana@globex.test
weft> cd /log/globex/prod
weft> cat *.log | head -1
2026-04-25T10:01:33.488Z INFO order_placed customer_id=c_91f ...    # globex/orders
weft> ls /log/globex/prod
billing.log  notifier.log  orders.log  billing_metrics.log  notifier_metrics.log  orders_metrics.log
```

Neither user sees the other's namespace. `ls /log/globex` from acme's shell should return empty; `cat /log/globex/prod/billing.log` should error with "unknown source".

### Env dimension stamping

As alice (acme):
```
weft> cat api.log | awk '$ENV == "prod"' | head -3
weft> cat api.log | awk '$ENV == "sand"' | head -3
weft> cat api.log | awk '$ENV == "staging"' | wc -l
0
```
First two have rows, third returns zero — acme has no `staging` env, even though globex does. Confirms the coordinator is honoring the token-bound env (migration 034) and not just trusting the agent or leaking env names across customers.

### Charts on a known shape — watch form

`chart <watch>` reads from a saved watch's recent results. The
existing `error-rate` watch fires every 5 minutes; pointing the
chart at it surfaces the rate-shape generator's curve:

```
weft> chart error-rate
weft> chart error-rate -f             # follow, redraws on tick
```

On `acme/api` (prod, `sine:20,40,600`) the chart should trace a
clean ~10-minute-period sine modulated by the `ERROR_RATIO`. On
`acme/worker` (sand, sawtooth) it should show ramp-and-snap. On
`globex/billing` (staging, `monotonic` with no reset) it should
climb steadily and the chart should auto-rescale as the y-axis
blows out. Side-by-side across two terminals, billing/staging
(unbounded) and billing/prod (bounded) should look identical
until prod snaps back.

### Charts ad-hoc — pipe form

The pipe-stage chart renders a one-shot pipeline without first
naming a watch. The seeded `/bin/mem_history` script reshapes a
metrics log into chart-friendly JSONL.

**Step 1.** Sanity-check the script's output shape:

```
weft> mem_history /log/acme/prod/api_metrics.log | head -3
{"ts":"2026-04-25T10:00:00.123456Z","pct":42.31}
{"ts":"2026-04-25T10:00:10.131829Z","pct":42.45}
{"ts":"2026-04-25T10:00:20.144117Z","pct":42.58}
```

JSONL with two keys per line. If the rows look right, pipe into
chart:

**Step 2.** Time-bucketed average memory percentage:

```
weft> mem_history /log/acme/prod/api_metrics.log \
    | chart -x .ts -y .pct --bucket 1m -a avg
```

Expect a line chart filling the terminal with a HH:MM x-axis (one
tick label per bucket window), a numeric y-axis on the left, and
~60 buckets across (1h ÷ 1m). With weftd's 10s metrics interval
each bucket aggregates ~6 samples.

**Step 3.** Swap the reduction; the same data renders as a different
shape:

```
weft> mem_history /log/acme/prod/api_metrics.log \
    | chart -x .ts -y .pct --bucket 1m -a p95
weft> mem_history /log/acme/prod/api_metrics.log \
    | chart -x .ts -y .pct --bucket 30s -a max
```

The `p95` chart should track noticeably higher than `avg`; the
`max` chart should be even higher and have twice as many buckets.

**Step 4.** Categorical x (no time bucketing). Default kind is
`bar` for categorical x, `line` for `--bucket`; `-t` overrides.

```
weft> cat /log/acme/prod/api.log \
    | jq '{path, latency}' \
    | chart -x .path -y .latency -a sum
```

Expect a horizontal-bar chart, one bar per distinct `.path` value
in the upstream output, sized by the total `.latency` sum across
matching rows. The path pool the generator uses is small (~5
endpoints), so the chart shows ~5 bars.

**Step 5.** Argument validation. Mixing positional with JSON-path
in the same chart call is rejected at parse time:

```
weft> mem_history /log/acme/prod/api_metrics.log | chart -x 1 -y .pct
chart: -x and -y must both be positional or both .json.path
```

No upstream query is dispatched — the error surfaces immediately
in scrollback.

### JSON-path alerts

The `mem-pressure` watch exercises `alert -k .pct -t .host`
against JSONL — no `jq -r` reformatting, the alert reads the JSON
field directly.

**Step 1.** Read back the watch definition:

```
weft> cat /cron.d/mem-pressure
schedule: */1 * * * *
tail -n 12 /log/acme/prod/api_metrics.log \
  | jq 'select(.type == "memory")' \
  | alert above 70 -k .pct -t .host
```

**Step 2.** Wait one tick window (≤ 1 minute) and check the
recent results — value is the parsed `.pct`, tag is the parsed
`.host`:

```
weft> weft.latest mem-pressure
65.4
weft> ls -l /cron.d/
mem-pressure   <colored sparkline>   65.4
error-rate     <colored sparkline>   210
scan
…
```

If `pct` ever crosses 70 the watch fires for that host (visible
on the dashboard's incidents page; in the shell, the next
`weft.latest` returns ≥70).

**Step 3.** Cross-customer isolation still holds. As dana@globex,
`weft.latest mem-pressure` returns globex's mem-pressure number,
not acme's. Reading the acme watch directly is rejected:

```
weft> cat /cron.d/mem-pressure
… globex's mem-pressure body, not acme's …
```

**Step 4.** The mixing-rejection rule is a parse-time guard for
both `chart` and `alert`. A quick interactive test confirms alert
enforces it:

```
weft> echo '{"host":"a","pct":85}' | alert above 80 -k 2 -t .host
alert: -k and -t must both be positional or both .json.path
```

No alert fires; the parse error returns exit 1 immediately.

### Metrics piggy-back

Each container reports its own cpu/memory/load into
`<namespace>/<deployment>_metrics` — one app per service. As alice:

```
weft> ls /log/acme/prod
api.log  api_metrics.log  edge.log  edge_metrics.log  worker.log  worker_metrics.log
weft> cat /log/acme/prod/api_metrics.log | jq 'select(.type == "cpu") | (100 - .idle)' | head -3
```

The metrics rate-shape is downstream of the log rate-shape (more
lines → more I/O → more CPU), so we should see the curves
correlate. To chart it, point a watch at the metrics stream:

```
# /cron.d/api-prod-cpu
# schedule: every 1m
cat /log/acme/prod/api_metrics.log | jq -r 'select(.type == "cpu") | (100 - .idle)' | tail -1
```

Then `chart api-prod-cpu -f`. As dana, the same query against
`globex/orders_metrics` works; `cat /log/acme/prod/api_metrics.log`
returns "unknown source" — same isolation as for normal logs.

### Authorship plumbing

```
weft> ls -al /bin
-rwxr-xr-x  alice    Apr 25 10:14  scan
-rwxr-xr-x  bob      Apr 25 10:14  count_errors
weft> wit log /bin/scan
* a4fb0c2  HEAD     2026-04-25 10:14  alice    "fix the awk filter"
  3d2e91f  HEAD^    2026-04-25 10:13  bob      "tighten error pattern"
  c01b745  HEAD^2   2026-04-25 10:12  alice    "initial pass"
```

Confirms migration 032 attribution + migration 036 history wiring end-to-end.

### Permissions

`carol@acme.test` (read-only) should be able to `cat *.log` and `cat /bin/scan` and `wit log /bin/scan`, but `vi /bin/scan` followed by `:w` should reject with the existing permissions error. No `agent_tokens` should be visible to her either (revoke is a write, but enumeration goes through `ListAgentTokens` which already filters by customer).

### Watch firing across tenants

The pre-seeded `error-rate` watch in each customer fires every 5m. After ~10m, both customers should have `cron_results` rows; running `weft.latest error-rate` as alice should never return globex's number, and vice versa.

## Out of scope

- **AWS/CloudWatch sources.** This seed is weftd-only. Adding a CloudWatch source needs a real AWS account and IAM, which we already exercise from the prod tooling; not needed for tenancy verification.
- **Notify configs / Slack / PagerDuty.** Stub `notify_configs` rows could be added later; alerting wire-out isn't what we're testing here.
- **Subscriptions / billing.** The two customers don't need a `subscriptions` row to function for this scenario; defaults are fine.
- **/tmp content.** Could be seeded too (the schema supports it via `tmp_files`), but adds noise without much demo value. Skip in v1.
- **Realistic time spread.** The generator runs in real time from `up`; we're not back-dating logs. If we want a "two-day-old" demo we re-up two days later or write a separate backfiller. Not needed for v1.

## Files to add

```
cmd/weft-demo-seed/
  main.go                  # idempotent seed CLI
  scripts.go               # /bin and /cron.d preseed content + wit version playback
dev/demo/
  Dockerfile               # weft-demo-service image
  entrypoint.sh
  generator.py             # text generator
  up.sh                    # convenience wrapper
  teardown.sh
  tokens.env.example       # documents the shape; tokens.env itself gitignored
docker-compose.demo.yml    # 12 service overlays
docs/cli/demo.md           # how to run the demo end-to-end
.gitignore                 # add dev/demo/tokens.env
```

## Open questions

- **Where does `tokens.env` live?** Right now I have it in `dev/demo/`. It's a generated artifact, gitignored, never checked in. Re-mintable by deleting the file and re-running the seed. Calling out so it's not a surprise — there's a real secret on disk after seed.
- **`monotonic` semantics — am I reading you right?** I treat it as "ramp from base at slope, optionally snap back at reset". When `reset` is set it overlaps with `sawtooth`; when omitted, it grows forever. Lets us pair an unbounded leak (billing/staging) against a bounded one (billing/prod) to verify chart auto-fit. If you meant something different — e.g. `monotonic` is for a *log-content* counter rather than the *rate*, or the climb is exponential rather than linear — say so before I write the generator.
- **Should the read-only user have a separate flow to provoke a permission error?** Adding "carol tries to save and gets denied" to the verification checklist would be nice but requires interactively invoking vi-then-:w from the shell. Probably fine to leave as an unguided "try it" rather than scripting.
- **Do we want pre-seeded `cron_results` rows, or wait for the watches to actually run?** Real runs make the demo more authentic but force a multi-minute wait after `up` before the dashboards have anything. Compromise: seed 6 hours of synthetic results per watch (ts spread, plausible numbers) so the dashboard has history immediately, *and* let real runs append on top.
