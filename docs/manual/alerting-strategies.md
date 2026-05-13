---
id: manual-alerting-strategies
title: Alerting strategies
web:
  section: alerting-strategies
  category: alert-visualize
  order: 53
---

This page is the catalogue of *shapes* an alert can take in weft —
single-watch scalar alerts, multi-row per-tag alerts, and composite
alerts that fire on the combined state of several watches. The
`alert` builtin's grammar lives in `man alert`; the `weft::*` family
that backs predicates and aggregates lives in **Weft functions**.
This page is about which shape fits which monitoring problem.

## The four shapes

### 1. Single-watch scalar

The most common form. The watch's pipeline ends in `| alert <cond>`;
each tick produces one numeric value that the alert evaluates.

```
# /cron.d/error-rate
# schedule: every 1m
tail -n 500 /log/api.log | grep ERROR | wc -l | alert above 50 --notify=ops
```

Use this when "is the metric above a known threshold?" answers
your question and the metric is one number.

### 2. Multi-row per-tag

When a tick produces *multiple* values keyed by some dimension —
"disk usage per host," "error count per service" — use the
`-k`/`-t` form of `alert`. Each row opens and closes incidents
independently against its tag.

```
# /cron.d/disk-per-host
# schedule: every 1m
tail /log/shard.log \
  | jq -r 'select(.type=="disk") | "\(._host) \(.pct)"' \
  | alert above 80 -k 2 -t 1 --notify=ops
```

`-k 2` says "field 2 holds the numeric value"; `-t 1` says "field
1 names this row" (the host). Each host opens and closes incidents
independently — you get one PagerDuty incident per affected host,
not one giant incident for "anything's disk is over 80%."

`-k` and `-t` also accept JSON paths, which skips the `jq -r`
reformatting:

```
tail /log/shard.log \
  | jq 'select(.type=="disk")' \
  | alert above 80 -k .pct -t ._host --notify=ops
```

A single alert may not mix the two forms — both `-k` and `-t` must
be positional, or both must be `.`-prefixed paths.

The same shape works with any indexed dimension as the tag — swap
`._host` for `._deployment`, `._pod`, `._region`, or any
combination (`"\(._region) \(._host) \(.pct)"` for region-prefixed
host names).

### 3. Composite (state across multiple watches)

When the alert depends on more than one watch — "page me when error-
rate is high **and** the canary stopped pinging" — compose
predicates with bash `&&` `||` `!` or Lua `and` `or` `not`, then
hand the resulting exit code to `alert` via `--if-zero "$?"`:

```bash
# /cron.d/deploy-danger
# schedule: every 1m
weft::is_above error-rate 50 && weft::is_absent canary-ping
alert --if-zero "$?" --notify=critical
```

`weft::is_above error-rate 50 && weft::is_absent canary-ping` exits
0 when both predicates fire (the bash convention for *success*);
`"$?"` expands to that exit code; `alert --if-zero "$?"` fires when
the expanded value is `"0"`.

`||` (OR) and `!` (NOT) work the same way:

```bash
weft::is_above error-rate 50 || weft::is_above payment-fail 0
alert --if-zero "$?" --notify=critical

! weft::is_absent maintenance-flag && weft::is_above error-rate 50
alert --if-zero "$?" --notify=critical   # fires only outside maintenance
```

The Lua surface mirrors the bash predicates one-for-one. Boolean
composition uses native `and` `or` `not`; emit `1` or `0` and pipe
to `alert above 0`:

```lua
-- /cron.d/deploy-danger
-- schedule: every 1m

if weft.is_above("error-rate", 50) and weft.is_absent("canary-ping") then
  print(1)
else
  print(0)
end
```

Pipe the cron's stdout through `| alert above 0 --notify=critical`,
or wrap with the bash `--if-zero` form. Lua wins when the chain
has more than two clauses, when the predicate uses computed values,
or when you want a meaningful debug print:

```lua
local rate = tonumber(weft.latest("error-rate")) or 0
local canary_dead = weft.is_absent("canary-ping")
local in_maintenance = not weft.is_absent("maintenance-flag")

if rate > 50 and canary_dead and not in_maintenance then
  io.stderr:write(string.format("composite fire: rate=%.1f canary_dead=%s\n",
    rate, tostring(canary_dead)))
  print(1)
else
  print(0)
end
```

### 4. Aggregated-value (windowed) alerts

When the question is "what does the value look like over a window"
rather than "what is it right now," use a `weft::*` aggregate in
the cron body and alert against its stdout:

```
# /cron.d/error-rate-hourly
# schedule: every 5m
echo $(weft::avg error-rate 1h) | alert above 50 --notify=ops
```

Or compose with the stdin comparators when chaining into a
composite:

```bash
weft::avg error-rate 5m | weft::is_gt 50 && weft::is_absent canary-ping
alert --if-zero "$?" --notify=critical
```

The aggregates' window caps (24h for `avg`/`min`/`max`/etc., 14d for
endpoint funcs) limit how far back the rolling window can reach.
For longer-horizon comparisons, write a watch that itself runs an
aggregate every 5 minutes — its stored history then *is* the
longer-horizon series.

## Trend / anomaly vs threshold

| Want to fire when... | Use |
|---|---|
| Value crosses a known threshold | `alert above` / `alert below` (or `weft::is_above` / `weft::is_below` in a composite) |
| Value differs from the previous tick at all | `weft::has_changed` |
| No result came in | `weft::is_absent` |
| Value rose/fell by N% vs recent baseline | `alert rising` / `alert falling` (or `weft::is_rising` / `weft::is_falling`) |
| Value is unusual for *this watch's recent history* | `weft::is_outlier` |
| Value is unusual for *this watch at this time of day/week* | `weft::is_seasonal` |

Use `is_outlier` when "what's normal" doesn't depend on the clock —
host memory, queue depth, error rate of a steady-state service. Use
`is_seasonal` when traffic has weekly/daily cycles — a Tuesday-2pm
threshold under-pages on Saturday at 3am and over-pages at the
nightly batch job.

## The absent-vs-zero gotcha

`weft::latest` of a missing watch returns the empty string;
`tonumber("")` in Lua returns `nil`; arithmetic against `nil`
errors. Always guard:

```lua
local v = tonumber(weft.latest("foo")) or 0   -- treat absent as zero
```

In bash, predicate builtins handle this for you — `weft::is_above
missing 50` exits 1 (no fire), it doesn't crash.

If you actually want to distinguish absent from zero, use
`weft::is_absent` explicitly in the chain:

```bash
weft::is_absent error-rate    # fires when there's no recent error-rate result
# ...whereas...
weft::is_above error-rate 0   # fires only when there IS a result AND it's > 0
```

## Debouncing flaps

Composite conditions tend to flap — each leg individually crosses
back and forth across its threshold, and the composite pops whenever
any pair simultaneously trips. The existing `alert --duration <dur>`
flag handles this: it requires the trigger to hold for the entire
window before firing. Once the composite output is a scalar (1 or 0
piped through alert), `--duration` works the same as for any other
alert.

```bash
weft::is_above error-rate 50 && weft::is_absent canary-ping
alert --if-zero "$?" --duration 5m --notify=critical
```

Five minutes of sustained composite-true before the page.

The same applies to multi-row alerts — `--duration` is per-tag, so
`alert above 80 -k 2 -t 1 --duration 5m` requires each host's
five-minute window to clear the threshold before that host's
incident opens.

## Anti-patterns

- **Don't fan out alerts from inside the chain.** A predicate
  builtin is a *test*, not a side-effect. If you want one cron to
  page multiple aliases, write multiple `/cron.d/` entries or use
  one `notify.yaml` alias that fans out to multiple destinations.
- **Don't put `weft::*` predicates on the right side of `|`.** They
  read state, not stdin. Pipe values through `weft::is_gt` /
  `weft::is_lt` for stdin-driven comparisons.
- **Don't compose state-mutating commands in the chain.** Keep the
  predicate composition side-effect-free. `weft::is_above ... &&
  curl https://my-deploy-bot/...` works but couples alerting to
  unrelated I/O and makes failure modes harder to reason about.
- **Don't reuse a single-row alert for multi-row data.** Multiple
  rows on stdin without `-k`/`-t` collapse into one scalar, which
  the alert will reject as non-numeric. Either reduce upstream with
  `wc -l` or `awk` (single-row), or add `-k`/`-t` (multi-row).

## See also

- **Weft functions** — full reference for the `weft::*` value funcs,
  predicates, and stdin comparators.
- **Watches** — cron watches and incident lifecycle.
- `man alert` — the alert builtin's full grammar.
- `docs/manual/notify.md` — alias and destination configuration.
