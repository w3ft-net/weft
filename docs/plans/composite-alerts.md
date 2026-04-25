# Plan: Composite alerts

Alerts that depend on more than one watch — "fire when A is above 50 AND B is absent." Mostly a documentation plan: the primitives already exist via `weft.*` helpers in watch bodies. A small helper (`weft.combine`) eliminates the boilerplate for the common AND / OR / NOT cases.

## Why this is small

Today a watch body is a pipeline; Lua is available via `#!/usr/bin/env lua`; every `weft.*` helper is callable from either language. That means the following already works:

```
# /cron.d/combined-prod-health
# schedule: every 1m
#!/usr/bin/env lua

local error_rate = tonumber(weft.latest("error-rate")) or 0
local canary_absent = weft.latest("canary-ping") == ""

if error_rate > 50 and canary_absent then
  print(1)
else
  print(0)
end
```

Pipe that through `alert above 0 --notify=critical` and you have a composite alert. No new feature.

What the customer actually asks for when they say "composite alerts" is:

1. **Discoverability** — they don't know this works. Today's manual doesn't teach it.
2. **Less boilerplate** — the Lua above is five lines of ritual to express "A and B". With >2 inputs it gets ugly fast.

Both are docs + a small helper. No new storage, no new scheduler, no new UI.

## What to build

### 1. `weft.combine(...)` — new shell helper

A variadic combinator that takes condition specs and returns 1 or 0, ready to pipe through `alert`.

```
# /cron.d/combined-prod-health
# schedule: every 1m
echo $(weft.combine \
  --all \
  "latest error-rate above 50" \
  "latest canary-ping absent") \
  | alert above 0 --notify=critical
```

Or in Lua, which is more readable once you hit three inputs:

```lua
local firing = weft.combine{
  mode = "all",   -- "all" = AND, "any" = OR, "none" = NOT(any)
  { watch = "error-rate",    cond = "above",  value = 50 },
  { watch = "canary-ping",   cond = "absent" },
  { watch = "queue-depth",   cond = "outlier", n = 3, window = "1h" },
}
if firing then print(1) else print(0) end
```

Each sub-condition reuses the same grammar as the existing `alert` command, minus dispatch — it just evaluates true/false against the watch's latest value (or its history, for `outlier`/`seasonal`/`rising`/`falling`).

Keeping the grammar symmetric with `alert` means one mental model, not two.

### 2. `docs/manual/composite-alerts.md`

Recipe page covering:

- The plain-Lua pattern (for customers who want full control).
- The `weft.combine` shortcut (for AND/OR/NOT).
- Why the "absent" component is tricky (empty string vs. zero), with the canonical check.
- A pattern for "fire only if both have been true for 5 consecutive ticks" using `weft.avg` over a short window.
- A non-pattern section: what NOT to try (multi-stage pipelines emitting side-effect alerts, which fire for the wrong reason and confuse dedup).

### 3. Debouncing / stability

Composite conditions tend to flap — A and B each flick above/below their individual thresholds independently, and the composite pops whenever they cross at the same moment. The existing `alert --duration 5m` option handles this already once the combined value is a scalar: the composite only fires if `weft.combine` returns 1 for every run in the window.

Doc should call this out prominently; customers will get noise if they don't.

## Components to build

1. **`shellcore/weft_combine.go`** (or wherever other `weft.*` helpers live) — implements `combine` in both call sites: as a shell command that reads string condition specs, and as a Lua function that takes tables. Uses the same condition evaluator as `alert` (factor out of `coordinator/alert.go` if needed).
2. **Grammar shared with `alert`** — same condition parser; each sub-condition is a scalar over `latest(watch)` or the window aggregates, yielding true/false.
3. **`docs/manual/composite-alerts.md`** — the recipe page.
4. **Cross-link** from `docs/shell/help-alerts.md` ("for conditions across multiple watches, see composite-alerts").
5. **Tests** — `shellcore/weft_combine_test.go`: all/any/none modes; missing watch; non-numeric inputs; interaction with `--duration`.

## Estimated effort

- Extract shared condition evaluator from `coordinator/alert.go`: **0.5 day**
- `weft.combine` bash + Lua bindings: **0.75 day**
- Manual page + cross-links: **0.5 day**
- Tests: **0.5 day**

**Total: ~2.25 days**. Half of that is the manual page; a composite alert can already be written today without any of it.

## Non-goals

- **No "composite incident" merging.** Each composite watch opens one incident of its own. We do not try to correlate "incident on watch A at t+0" with "incident on watch B at t+2" as a single super-incident. Customers who want that composition write a composite watch.
- **No visual condition builder in the UI.** The watch body is text; composite watches are text. If we ever build a no-code alert builder, it lives on top of this primitive, not under it.
- **No first-class "depends on" graph in the store.** The composite watch just reads from other watches via `weft.*`; there's no schema-level graph to traverse, and therefore no cycle detection. If a customer references their own composite watch by name, `weft.latest` returns the last run's result and the eval is well-defined (just one-step-behind). No need to prevent cycles.
- **No per-sub-condition notify targets.** The composite fires as one, dispatches as one. Granular dispatch is what regular single-watch alerts are for.
