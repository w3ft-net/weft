# Plan: Alert silencing and maintenance windows

Customer-configurable silences that suppress alert dispatch for a bounded time window, matched against a watch-name or notify-alias pattern. Covers the three real cases — planned maintenance, deploys, and known-broken-now situations where pages are already flowing and we want them to stop.

## Why add this

Today's dispatch path in `coordinator/notify.go` is unconditional: if the alert condition holds and the notify alias resolves, the notification fires. There's no way to say "we're doing a deploy at 3am; don't page anyone for the api watches for 30 minutes." Customers have two unsatisfying workarounds:

1. Rip the `--notify=` out of every affected watch, redeploy the watches, and reinstate them after. Error-prone.
2. Set aggressive alert conditions that won't fire during the window. Defeats the purpose.

A silence primitive is the standard fix and the minimum thing we can do that removes the workaround.

## Architecture

Silences are per-customer rows with:

- One or both of `watch_pattern` / `alias_pattern` (shell-style glob over the respective name).
- A time bound: either `starts_at`/`ends_at` (one-shot) or a cron-like recurring schedule (`weekly Mon 03:00-04:00 America/New_York`).
- An optional `reason` string that propagates into the dispatch log so the silence shows up in "why didn't we page?" postmortems.

The notifier consults the silence table before dispatching:

```
Notifier.Send(customerID, aliasName, watchName, body)
  → if any active silence matches (aliasName, watchName) at now():
      log "suppressed: <silence.reason>"
      return nil   (no dispatch, no error)
  → otherwise: existing dispatch loop
```

Incidents still open and close the same way — the condition evaluation is unchanged, only the outbound page is suppressed. When the silence ends, a still-firing incident gets a "resumed after silence" dispatch.

## Components to build

1. **`store.AlertSilence`** — new model: `{id, customer_id, watch_pattern, alias_pattern, starts_at, ends_at, cron_expr, tz, reason, created_by, created_at}`. Either `starts_at/ends_at` or `cron_expr` is populated; the active-at-now check resolves both shapes.
2. **`coordinator/notify.go`** — silence lookup in `Send()`. Results are cached for ~30s per customer to keep the hot path cheap.
3. **Shell** — `vi /etc/silences.conf` YAML, list of silence entries. Save-time validation rejects bad cron expressions and overlapping one-shots.
4. **CLI shortcut** — `weft silence add --duration 30m --alias ops --reason "deploy"` for the common ad-hoc case without hand-editing the YAML.
5. **Web UI** — Account → Silences. List of active + upcoming silences; one-click extend / cancel on an active silence.
6. **Dispatch audit** — every suppressed dispatch writes a row to `notify_log` with `{watch, alias, silence_id, suppressed_at}` so "why didn't I get paged?" is one query away.
7. **Docs** — `docs/manual/alert-silencing.md`, plus a cross-link from `docs/shell/help-alerts.md`.

## YAML shape

```yaml
# /etc/silences.conf

- reason: "nightly batch window"
  alias: ops
  cron: "0 3 * * *"
  duration: 1h
  timezone: America/New_York

- reason: "deploy v2.14 — paul"
  watch: "api-*"
  starts_at: 2026-04-24T14:00:00Z
  ends_at:   2026-04-24T14:30:00Z

- reason: "incident #142"
  alias: critical
  duration: 4h     # from now
```

`watch:` and `alias:` are globs; `watch: "*"` plus `alias: "*"` blankets everything, which we'll log prominently in audit so it doesn't become invisible.

## Estimated effort

- `AlertSilence` model + active-at-now resolver (with cron): **0.75 day**
- Notifier hook + cache: **0.5 day**
- Shell YAML + validation: **0.5 day**
- CLI shortcut: **0.25 day**
- Web UI (list, extend, cancel): **1 day**
- Audit log entries + surface: **0.5 day**
- Docs + tests: **0.5 day**

**Total: ~3.5 days**.

## Non-goals

- **No global "pause all alerts" switch.** Silences are patterned; a customer-wide panic button hides too much. If someone wants it, they write `watch: "*"` with a reason.
- **No per-user silences.** Silences are customer-scoped; everyone on the team sees the same dispatch suppression. Per-user mute belongs in the notify-destination config (e.g., "don't email me between 10pm and 6am") — separate plan if it ever surfaces.
- **No dependency on notify aliases having a shared state machine.** Each alias dispatches independently; the silence just wraps the outbound call.
- **No "silence on deploy" hook.** We don't auto-create silences from deploy events. If a customer wants that, they call `POST /api/silences` from their deploy script using the existing API token.
