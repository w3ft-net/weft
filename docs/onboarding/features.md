# User-Facing Features

This section walks through weft's major user-facing features, how
each is implemented, and where the code lives. Features are
presented roughly in the order a user encounters them.

## Log querying

The core feature. Users query logs through Unix pipeline syntax:

```
cat /log/myco/api/main.log | grep ERROR | awk '{print $3}' | head -20
tail -f /log/myco/api/main.log | grep "timeout"
```

Log sources appear as pseudofiles under `/log/`. The path
hierarchy is `/log/<namespace>/<service>/<kind>.log` (a
`<env>` segment slots in after the namespace for env-bearing
tokens). Users can browse with
`ls /log/`.

**Implementation path:**

1. Shell parses the command → `bash.Parse()` → AST.
2. Query rewriter detects `/log/` source →
   `coordinator/query_rewrite.go`.
3. Planner extracts pushdown predicates →
   `planner/planner.go`.
4. Coordinator fans out SQL to shards →
   `coordinator/coordinator.go`.
5. Compute pool runs remaining stages →
   `compute/engine.go`.
6. Output streams to the user's terminal.

For `tail -f`, the coordinator subscribes to the live broker
and streams new rows through the pipeline stages in real time.

## Meta-variables

Row-level dimension columns that map to real DuckDB columns,
enabling fast predicate pushdown. Documented in
`docs/manual/metavars.md`.

In awk, meta-variables are all-caps flat names:

```
awk 'LEVEL == "ERROR" && HOST ~ "api-"'
```

In jq, they use leading-underscore lowercase:

```
jq 'select(._level == "ERROR") | ._host'
```

Each meta-variable rewrites to a direct `WHERE` clause. The
planner handles this in `planner/awk.go` — it maps `LEVEL` to
the `level` column, `HOST` to `instance`, etc.

Key meta-variables: `NAMESPACE`, `SERVICE`, `KIND`, `HOST`,
`LEVEL`, `TS`, `DEPLOYMENT`, `ENV`, `VERSION`, `REGION`,
`CLUSTER`, `K8S_NAMESPACE`, `POD`, `CONTAINER`, `NODE`.

## Crons

A cron is a named, scheduled pipeline. It runs on a cron
schedule, captures stdout as the result, and can trigger alerts.

```
vi /cron.d/error-count
```

Cron file format:

```
# schedule: */5 * * * *
# notify: ops
tail -n 500 /log/myco/api/main.log | awk 'LEVEL == "ERROR"' | wc -l
```

**Implementation:**

- Model: `store/watch.go` (or related store files).
- Scheduling: the coordinator's cron executor runs crons on
  their schedule.
- Results: stored in a `cron_results` table with timestamps,
  enabling time-series queries.
- Version history: every save records a version in
  `cron_version_history`. The `git` command (a subset of git, see `witrepo/`) reads and writes these.
- Filesystem: `shellfs/shellfs.go` handles reads/writes to
  `/cron.d/` paths, validates the schedule header, and delegates
  to the store.

### Result accessors

Crons produce history that other crons and scripts can query:

| Helper | Returns |
|---|---|
| `weft.latest(cron)` | Most recent stdout (trimmed) |
| `weft.avg(cron, dur)` | Mean of numeric results in window |
| `weft.max(cron, dur)` | Max in window |
| `weft.min(cron, dur)` | Min in window |
| `weft.sum(cron, dur)` | Sum in window |
| `weft.stddev(cron, dur)` | Population stddev (≥2 samples) |

These are implemented in `coordinator/weftcmds.go` and backed by
store queries.

## Alerts

Alerts are pipe stages appended to a pipeline. They evaluate the
output against a condition and dispatch notifications.

```
cat /log/myco/api/main.log | grep ERROR | wc -l | alert above 50 --notify=ops
```

Conditions: `above`, `below`, `eq`, `changed`, `rising N%`,
`falling N%`.

**Implementation:**

- Parsing: alert arguments are parsed from the pipeline AST.
  The planner peels off the alert stage and returns it separately
  in `QueryPlan.AlertArgs`.
- Evaluation: `coordinator/alert.go` evaluates the condition
  against the pipeline output after execution.
- Notification: `coordinator/notify.go` dispatches to channels
  defined in `/etc/notify.yaml` (Slack, PagerDuty).
- Multi-row alerts: the `-k` flag groups results by a key column,
  and `-t` sets the threshold per group.

## Dashboards

Dashboards compose existing crons into a tile layout. They are
YAML files under `/etc/dashboards/`:

```
vi /etc/dashboards/prod.yaml
```

```yaml
title: Production
sections:
  - title: Errors
    tiles:
      - kind: sparkline
        watch: error-rate-hourly
        size: md
      - kind: counter
        watch: pending-jobs
        format: "%d jobs"
        threshold: { warn: 100, crit: 500 }
```

Tile kinds: `sparkline` (time-series chart), `counter` (single
number with coloring), `gauge` (0-max arc), `table` (columnar
output).

**Implementation:**

- Model and validation: `store/dashboard.go`. YAML is parsed and
  validated at save time — all `kind:` values must be recognized,
  all `size:` values must be sm/md/lg, every referenced cron must
  exist, and tables must declare `columns:`.
- Terminal rendering: `dashboard/render.go`. Greedy row-packing
  based on tile size hints and terminal width.
- Browser rendering: `frontend/src/pages/Dashboards.tsx`. CSS
  grid with responsive breakpoints (6 tracks wide, 2 tablet, 1
  phone). `sm`=2 tracks, `md`=3, `lg`=6.
- Filesystem: `shellfs/shellfs.go` handles `/etc/dashboards/`
  reads and writes with YAML validation on write.
- Tests: `store/dashboard_test.go`, `shellfs/dashboards_test.go`,
  `dashboard/render_test.go`.

## Charts

Charts render cron data as in-terminal visualizations.

```
chart error-rate-hourly
cat /log/myco/api/main.log | grep ERROR | wc -l | chart
```

**Implementation:**

- Registration: `chart/builtin.go` registers three hooks on
  shellcore — a full-screen view, a command-line handler, and a
  pipe-stage renderer.
- Frontend: `frontend/src/pages/Watch.tsx` renders Recharts-based
  line charts with range selection, zoom, per-tag legends, and
  threshold reference lines.
- Sparkline component: `frontend/src/components/Sparkline.tsx`
  for compact inline charts (used in dashboard tiles and cron
  listings).

## Scripts (/bin/)

User-defined bash or lua scripts stored in the virtual filesystem:

```
vi /bin/check-errors
```

Scripts support positional parameters, `weft.*` helpers, and the
full pipeline toolkit. Lua scripts start with a `#!/usr/bin/env
lua` shebang.

**Implementation:**

- Filesystem: `shellfs/shellfs.go` handles `/bin/` reads/writes.
- Execution: the coordinator loads the script body, parses it with
  `bash.Parse()`, applies the `BinScriptTransform` (query
  rewriter), and executes it.
- Version history: same `git` integration as crons.
- Store: scripts are rows in the `shell_functions` table, scoped
  to `customer_id`.

## MCP tools

Weft exposes an MCP (Model Context Protocol) server that lets AI
assistants query logs and propose changes through a constrained
interface.

**Implementation:** `cmd/weft/mcp/`

Read-only tools:

- `weft.run` — execute a pipeline (allowlisted verbs only, no
  redirects or control flow).
- `weft.ls` — list `/log/` paths.
- `weft.cat` — read log content.
- `weft.crons_list` — list every scheduled cron with its
  schedule and pipeline (audit / recover-before-propose).

Mutation tools (staged, human-approved):

- `weft.script_propose`, `weft.cron_propose`,
  `weft.dashboard_propose` — stage a change and return a diff.
- `weft.proposal_delete` — stage a deletion.
- `weft.proposals_list` — list pending proposals.
- `weft.proposals_apply` — commit proposals after review.

Policy enforcement: `cmd/weft/mcp/policy/` validates pipelines
through warp's bash AST, rejecting anything off the allowlist.

Log output is wrapped in `<weft-data>` blocks to signal untrusted
data to the model.

## The help system

Two surfaces:

- **Shell:** `help <topic>` and `man <page>` commands, backed by
  pre-generated ANSI text in `shell/help_corpus_gen.go`. Rendered
  via `shell/help_provider.go`.
- **Web:** `/manual` page with HTML generated from the same
  markdown sources. Rendered in `frontend/src/pages/Manual.tsx`.

See the [docgen section](docgen.md) for the full pipeline.

## Frontend pages

All in `frontend/src/pages/`:

| Page | What it does |
|---|---|
| `Shell.tsx` | WebAssembly shell with xterm.js |
| `Dashboards.tsx` | Multi-dashboard viewer with sidebar switcher |
| `Watch.tsx` | Cron detail with Recharts time-series |
| `Account.tsx` | Team, subscription, and integration management |
| `Namespaces.tsx` | CloudWatch source management |
| `Manual.tsx` | Embedded documentation viewer |
| `Device.tsx` | Agent token management |
| `Downloads.tsx` | CLI download page |

---

## Challenge: add a new notify destination

**Goal:** implement webhook notification dispatch — the simplest
of the four new destinations described in
`docs/plans/notify-destinations.md`.

This is a real feature from the roadmap. It touches the store,
the coordinator, and the documentation.

### Setup

Read these files first:

- `docs/plans/notify-destinations.md` — the full design. Focus
  on the webhook section only (ignore email, Opsgenie, Teams).
- `coordinator/notify.go` — the existing notification dispatcher.
  Understand how it fans out to Slack and PagerDuty today.
- `store/` — find where `NotifyAlias` is defined and how
  `/etc/notify.yaml` is parsed. Understand the existing shape.
- `docs/shell/man-alert.md` — the current alert documentation.

### Task

1. **Extend the notify alias model.** Add a `Webhook` list to
   `NotifyAlias`. Each entry is either a bare URL string or a
   `{url, headers}` mapping. Update `ParseNotifyAliases` to
   accept the new shape.

2. **Implement `sendWebhook`.** In `coordinator/notify.go`, add a
   function that POSTs a JSON payload to the webhook URL:

   ```json
   {
     "watch": "error-count",
     "alias": "ops",
     "ts_unix": 1717689600,
     "severity": "firing",
     "body": "52"
   }
   ```

   Use a 10-second timeout. If the webhook URL returns non-2xx,
   surface the error the same way Slack errors are surfaced.

3. **Wire it into the dispatcher.** In the existing fan-out loop
   that iterates over an alias's destinations, add the webhook
   case.

4. **Write a test.** Add a test in `coordinator/` that starts an
   `httptest.Server`, configures a notify alias with a webhook
   pointing at it, triggers an alert, and asserts the received
   JSON payload.

5. **Update the docs.** Add the `webhook:` key to the alert man
   page (`docs/shell/man-alert.md`) and regenerate with
   `go run ./cmd/docgen`.

### What you'll learn

- The end-to-end alert flow: pipeline → condition evaluation →
  notification dispatch.
- How `/etc/notify.yaml` is parsed and stored.
- The fan-out pattern in the notifier.
- How to add a feature that spans store, coordinator, and docs.
