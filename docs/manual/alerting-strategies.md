---
id: manual-alerting-strategies
title: Alerting strategies
web:
  section: alerting-strategies
  category: analysis-alerting
  order: 53
---

This page is the catalogue of *shapes* an alert can take in weft —
single-cron scalar alerts, multi-row per-tag alerts, composite
alerts that fire on several conditions at once, and
aggregated-value alerts over a window. The `alert` builtin's grammar
lives in `man alert`; the `weft::*` family that backs predicates and
aggregates lives in **Weft functions**. This page is about which
shape fits which monitoring problem.

A cron run reads only the logs from its own schedule interval: a
`*/5` cron sees the last five minutes. The recipes here count with
`grep ... | wc -l` and do not limit rows with `head -n` or
`tail -n`: a line count caps the rows read, so on a busy interval it
truncates the window and the cron undercounts. (At the prompt, `tail -n 500
| awk 'LEVEL=="ERROR"' | wc -l` counts errors among the last 500 rows
only, and `grep ERROR | tail -n 5` is the last five matches.) To try a
recipe interactively, set the window with `range` (see `man range`) and
run the pipeline without its `alert` stage, or with `alert` as the last
stage to see whether it fires.

Every example sends its alert to an alias with `--notify=<alias>`.
Aliases (`ops`, `critical` and so on) are defined in
`/etc/notify.yaml`; see **Notifications**.

## The four shapes

### 1. Single-cron scalar

The most common form. The cron's pipeline ends in `| alert <cond>`;
each tick produces one numeric value that the alert evaluates.

```
# /cron.d/error-rate
# schedule: every 1m
cat /log/myco/prod/api/main.log | grep ERROR | wc -l | alert above 50 --notify=ops
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
cat /log/myco/prod/api/disk.log \
  | jq -r '"\($host) \(.pct)"' \
  | alert above 80 -k 2 -t 1 --notify=ops
```

`-k 2` says "field 2 holds the numeric value"; `-t 1` says "field
1 names this row" (the host). Each host opens and closes incidents
independently, giving one PagerDuty incident per affected host.

`-k` and `-t` also accept JSON paths, which skips the `jq -r`
text formatting. The host is a column of the row, not a field in its
JSON, so build the row with it, using `jq -c` so each row stays on one
line; multi-line `jq` output is read one line per row:

```
cat /log/myco/prod/api/disk.log \
  | jq -c '{host: $host, pct}' \
  | alert above 80 -k .pct -t .host --notify=ops
```

A single alert may not mix the two forms — both `-k` and `-t` must
be positional, or both must be `.`-prefixed paths.

The same shape works with any field of the row as the tag — swap
`.host` for `.deployment` or `.path`. The tag is one token, so to
combine fields use the positional form and join them without a
space: `"\(.deployment)/\(.host) \(.pct)"` gives tags like
`api/web-1`.

### 3. Composite (more than one condition)

When the alert depends on more than one thing — "page me when errors
are high **and** the canary stopped pinging" — there are two ways to
combine conditions. Both end in a single pipeline whose last stage is
`alert`, because `alert` works only as the last (or only) command of a
script.

**Over the cron's time range.** Read every source the condition
needs, tally each leg in one `awk` pass, and print `1` when the whole
condition holds and `0` otherwise. Use this when the legs are visible
in the logs themselves:

```
# /cron.d/deploy-danger
# schedule: every 1m
cat /log/myco/prod/api/main.log /log/myco/prod/canary/main.log \
  | awk 'LEVEL=="ERROR"{e++} SERVICE=="canary"{c++}
         END{print (e>50 && c+0==0) ? 1 : 0}' \
  | alert above 0 --notify=critical
```

`e` counts errors and `c` counts canary lines in the cron's interval;
`c+0==0` is true when no canary line arrived at all. `&&` is AND,
`||` is OR and `!` is NOT inside the `awk` expression:
`print (e>50 || p>0) ? 1 : 0` fires when either leg is high, and
`print (e>50 && !m) ? 1 : 0` fires only when the `m` leg is zero (say,
a maintenance marker).

**Across crons' history.** When the legs are the stored results of other
crons, compose the `weft::*` predicates with bash `&&` `||` `!` in a
subshell, end it with an `echo`, and pipe that into `alert`. The
subshell prints `fire` only when the whole condition holds, and
`alert`'s default `any` condition fires on any output:

```
# /cron.d/deploy-danger
# schedule: every 1m
(weft::is_above error-rate 50 && weft::is_absent canary-ping && echo fire) | alert --notify=critical
```

`weft::is_above error-rate 50 && weft::is_absent canary-ping` succeeds
(exit 0, the bash convention for *success*) when both predicates fire,
so the `echo` runs. `weft::is_absent` fires when the named cron has no
result: it doesn't exist, has never run, or its latest output is
empty (with a window, such as `weft::is_absent canary-ping 10m`: no result
landed in it). (A bad window, one over the 24h cap, or a history that can't
be read doesn't fire; in a bash predicate the reason is printed on
stderr.)

`||` (OR) and `!` (NOT) work the same way:

```
(weft::is_above error-rate 50 || weft::is_above payment-fail 0 && echo fire) | alert --notify=critical

(! weft::is_absent maintenance-flag && weft::is_above error-rate 50 && echo fire) | alert --notify=critical   # fires only outside maintenance
```

Lua mirrors the bash predicates one-for-one, with native `and` `or`
`not`. Lua lives in a `/bin/` script (crons are bash), which prints `1`
or `0` and which the cron pipes into `alert above 0`. Use Lua when the
chain has more than two clauses, when the predicate uses computed values,
or when you want a meaningful debug print:

```lua
#!/usr/bin/env lua
-- /bin/deploy_danger
local rate = 0
if not weft.is_absent("error-rate") then
  rate = tonumber(weft.latest("error-rate")) or 0
end
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

```
# /cron.d/deploy-danger
# schedule: every 1m
deploy_danger | alert above 0 --notify=critical
```

### 4. Aggregated-value (windowed) alerts

When the question is "what does the value look like over a window,"
use a `weft::*` aggregate in the cron body and alert against its
stdout:

```
# /cron.d/error-rate-hourly
# schedule: every 5m
echo $(weft::avg error-rate 1h) | alert above 50 --notify=ops
```

Or compose with the stdin comparators when chaining into a
composite:

```
(weft::avg error-rate 5m | weft::is_gt 50 && weft::is_absent canary-ping && echo fire) | alert --notify=critical
```

The aggregates' window caps (24h for `avg`/`min`/`max`/etc., 14d for
endpoint funcs) limit how far back the rolling window can reach.
For longer-horizon comparisons, write a cron that itself runs an
aggregate every 5 minutes — its stored history then *is* the
longer-horizon series.

## Trend, anomaly and threshold conditions

| Want to fire when... | Use |
|---|---|
| Value crosses a known threshold | `alert above` / `alert below` (or `weft::is_above` / `weft::is_below` in a composite) |
| Value differs from the previous tick at all | `weft::has_changed` |
| No result came in | `weft::is_absent` |
| Value rose/fell by N% vs recent baseline | `alert rising` / `alert falling` (or `weft::is_rising` / `weft::is_falling`) |
| Value is unusual for *this cron's recent history* | `weft::is_outlier` |
| Value is unusual for *this cron at this time of day/week* | `weft::is_seasonal` |

The functions take different arguments:

- `weft::is_rising <cron> [<pct>] [<window>]`, and `weft::is_falling`
  likewise. `<pct>` is a bare number such as `50`; it takes no `%` suffix.
- `weft::is_outlier <cron> <n_stddev> <window>`
- `weft::is_seasonal <cron> <n_stddev> <period> <window>`

Windows and periods are Go durations plus `d` (24 hours) and `w` (7
days), and they combine: `30m`, `1h`, `1d`, `1w`, `1d12h`. See **Weft
functions** for the caps on each function.

Use `is_outlier` when "what's normal" doesn't depend on the clock —
host memory, queue depth, error rate of a steady-state service. Use
`is_seasonal` when traffic has weekly/daily cycles — a Tuesday-2pm
threshold under-pages on Saturday at 3am and over-pages at the
nightly batch job.

## Absent and zero values

A cron with no usable value and a value of zero are different cases. In
bash, `weft::latest` of a cron that doesn't exist is an error; in Lua, `tonumber("")` returns `nil` and arithmetic against
`nil` errors. Always check for absence and guard the conversion:

```lua
local v = 0   -- treat absent as zero
if not weft.is_absent("foo") then
  v = tonumber(weft.latest("foo")) or 0
end
```

In bash, predicate builtins handle this for you — `weft::is_above
missing 50` exits 1 (no fire).

To distinguish absent from zero, use
`weft::is_absent` explicitly in the chain:

```
weft::is_absent error-rate    # exit 0 (fires) when error-rate has no result, or doesn't exist
# ...whereas...
weft::is_above error-rate 0   # fires only when there IS a result AND it's > 0
```

## Debouncing flaps

Composite conditions tend to flap — each leg individually crosses
back and forth across its threshold, and the composite pops whenever
any pair simultaneously trips. The `alert --duration <dur>`
flag handles this: it requires the trigger to hold for the entire
window before firing. Once the composite output is a scalar (1 or 0
piped through alert), `--duration` works the same as for any other
alert. (`--duration` takes `s`, `m` and `h`; `1d` is rejected.)

```
(weft::is_above error-rate 50 && weft::is_absent canary-ping && echo 1 || echo 0) | alert above 0 --duration 5m --notify=critical
```

Five minutes of sustained composite-true before the page.

The same applies to multi-row alerts — `--duration` is per-tag, so
`alert above 80 -k 2 -t 1 --duration 5m` requires each host's
five-minute window to clear the threshold before that host's
incident opens.

## Anti-patterns

- **Don't fan out alerts from inside the chain.** A predicate
  builtin is a test and has no side effects. To page multiple
  aliases from one cron, write multiple `/cron.d/` entries or use
  one `notify.yaml` alias that fans out to multiple destinations.
- **Don't put `weft::*` predicates on the right side of `|`.** They
  read stored state. For stdin-driven comparisons, pipe values
  through `weft::is_gt` / `weft::is_lt`.
- **Don't compose state-mutating commands in the chain.** Keep the
  predicate composition side-effect-free: join `weft::*` predicates
  with `&&`, `||` and `!`, and nothing else. Hanging an unrelated
  command off the chain couples alerting to unrelated I/O and makes
  failure modes harder to reason about.
- **Don't reuse a single-row alert for multi-row data.** Multiple
  rows on stdin without `-k`/`-t` collapse into one scalar, which
  the alert will reject as non-numeric. Either reduce upstream with
  `wc -l` or `awk` (single-row), or add `-k`/`-t` (multi-row).

## See also

- **Weft functions** — full reference for the `weft::*` value funcs,
  predicates, and stdin comparators.
- **Crons** — scheduled pipelines and incident lifecycle.
- `man alert` — the alert builtin's full grammar.
- **Notifications** — alias and destination configuration.
