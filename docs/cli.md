# Weft CLI Design

## Overview

`weft` is an interactive shell for querying and tailing logs. It presents log sources as pseudofiles and supports standard Unix pipeline idioms via the warp bash interpreter. There is no separate `weft query` or `weft shell` subcommand — running `weft` drops you into the shell.

## Authentication

Token stored in `~/.weft.yaml`:

```yaml
endpoint: https://weft.example.com  # or http://localhost:8080 for dev
token: wft_abc123...
```

## Invocation

```bash
# Connect to all apps, default time range
weft

# Scope to specific apps
weft --app vimgym,callbox

# Dev/local
weft --endpoint http://localhost:8080
```

## Pseudofiles

Inside the shell, log sources appear as files:

| File | Contents |
|------|----------|
| `vimgym.log` | Logs from the vimgym application |
| `callbox.log` | Logs from the callbox application |
| `warp.log` | Aggregate of all apps included via `--app` (or all apps if unscoped) |

The list of available pseudofiles is determined at connect time from the user's account. New apps appear automatically.

Pseudofiles are not a filesystem abstraction — they are source declarations consumed by the query planner. `cat`, `head`, `tail`, and `tail -f` are custom implementations injected into the weft bash interpreter. When the interpreter encounters `cat vimgym.log | awk 'LEVEL == "ERROR"'`, it does not produce text and pipe it — the query planner analyzes the full pipeline as one unit, folds the awk predicate into a SQL WHERE clause, and sends a single query to the appropriate shards:

```sql
SELECT ... FROM logs
WHERE app = 'vimgym' AND level = 'ERROR'
  AND ts BETWEEN '...' AND '...'
```

Only pipeline stages that cannot be pushed down (formatting, complex aggregations) run as gpipe compute stages on the result stream.

## Shell usage

Standard Unix idioms work because the warp bash interpreter handles pipes, awk, sed, jq natively:

```bash
# Historical queries
cat vimgym.log | awk 'LEVEL == "ERROR"'
cat warp.log | jq 'select(.status == 500)' | head -20
cat callbox.log | grep timeout | wc -l
head -50 vimgym.log
tail -100 vimgym.log

# Live tailing
tail -f vimgym.log
tail -f vimgym.log | awk 'LEVEL == "ERROR"' | jq '.user_id'
tail -f warp.log | awk 'LEVEL == "ERROR" { count[SERVICE]++ } END { for (a in count) print a, count[a] }'

# Time range override (in-shell)
range 2h..
range yesterday..today
range 2026-03-28T00:00:00..2026-03-28T06:00:00
cat vimgym.log | awk 'LEVEL == "ERROR"'
```

## Meta-variables

Since weft controls the awk interpreter, metadata is injected as special variables alongside `$1`, `$2`, etc:

| Variable | Source |
|----------|--------|
| `NAMESPACE` | Namespace |
| `SERVICE` | Service name |
| `KIND` | Stream kind |
| `LEVEL` | Log level |
| `TS` | Timestamp |
| `INSTANCE` | Instance ID |

For jq, metadata is injected as top-level keys prefixed with `_`:

```bash
cat vimgym.log | jq 'select($level == "ERROR") | {ts: $ts, msg: .message}'
```

## Commands

### Pseudofile access

- `cat <file>` — all lines in current time range
- `head [-N] <file>` — first N lines (default 10)
- `tail [-N] <file>` — last N lines (default 10)
- `tail -f <file>` — live tail with push from active shard
- `trace <id>` — every log line carrying this `trace_id`, across every app/service (no `<file>` argument — a trace isn't scoped to one app). Historical only, no `-f`. A bare `trace <id>` prints a one-line summary (line/service count) followed by each row prefixed with a colored `[namespace/service]` badge; `NAMESPACE`/`SERVICE` are always available as real, filterable meta-variables even without the badge (e.g. `trace <id> | awk 'SERVICE == "api"'`).

All of these can be piped through awk/sed/jq/grep/sort/uniq/wc. The query planner pushes down as much of the pipeline as possible into the shard query.

### Built-in shell commands

- `range <duration>` / `range <start> <end>` — set/show time window
- `ls /log` — browse available apps (namespaces and their pseudofiles)
- `schema [app]` — show detected schema for an app
- `exit` / `ctrl-d` — quit

### Source onboarding (`weft source …`)

Top-level subcommands (not in-shell) for managing weftd/CloudWatch sources.
Every command below is source-type-agnostic except `add`, which only
creates CloudWatch push sources (weftd sources register themselves the
first time the agent connects; there is no `weft source add` equivalent
for them). See `docs/ingestion-weftd.md` for the sampling → pending →
configured lifecycle both source types share, and
`docs/ingestion-cloudwatch.md` for the CloudWatch push design.

- `weft source add --namespace N --service NAME --log-group GROUP [--region REGION]` — register a CloudWatch log group→service mapping, `StatusPending`. This only records the mapping; it does not deploy anything in your AWS account — see `docs/manual/cloudwatch.md` for the CloudFormation stack that actually forwards events. `--region` defaults to `us-east-2`.
- `weft source list` — list every configured and pending source for the caller's customer, with type (`weftd`/`kinesis`/`direct`), status, and the underlying log group / agent path.
- `weft source remove --namespace N --service NAME` — delete a source. Already-ingested rows stay in the shards; only future ingestion stops.
- `weft source pending [--namespace N]` — list sources awaiting configuration. A source still accumulating its sample shows as `sampling` with an `N/target` count.
- `weft source configure <ns/service>` — review the sample and approve a schema (interactive). Also used to swap the schema after a resample — the source keeps ingesting until the new schema is approved.
- `weft source schema <ns/service>` — print the live schema for a configured source. For a source still sampling, prints `<source> hasn't emitted enough lines to sample yet (N/target)` instead.
- `weft source resample <ns/service>` — request a fresh sample of an already-configured source. Ingestion continues on the current schema while the agent re-samples; the new sample appears for review when it's ready.

## Open questions

- Prompt format: `weft>`, `weft(vimgym,callbox)>`, something else?
- Tab completion for pseudofile names and meta-variables?
- History persistence across sessions?
