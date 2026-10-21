---
id: manual-weft-funcs
title: Weft functions
web:
  section: weft-funcs
  category: analysis-alerting
  order: 54
---

The `weft::*` family reads and reasons about your own cron history
from inside a pipeline. There are three
shapes:

- **Value funcs** return the most recent value, an aggregate over a
  window, or an anchor like the slope between two endpoints.
- **Predicates** return a boolean — "is this cron above 50?",
  "did it just change?" — for composing alert conditions across
  multiple crons.
- **Stdin comparators** turn a piped value into a boolean.

The same set is available in two host languages:

| Surface | Syntax | Example |
|---|---|---|
| Bash builtin | `weft::name args` | `weft::avg error-rate 1h` |
| Lua function | `weft.name(args)` | `weft.avg("error-rate", "1h")` |

Bash uses `::` as the namespace separator; Lua uses `.` for table
access. The arguments are the same in both.

For composing predicates into composite alerts, see
**Alerts**.

## Value functions

Each takes a cron name, returns a value, and (with the exception of
`weft::latest` and `weft::previous`) a duration window. Durations are a
number and a unit: `30s`, `5m`, `1h`, `6h`, plus `d` for a day (24
hours) and `w` for a week (7 days). Units combine: `1d12h`.

| Bash | Lua | Returns | Notes |
|---|---|---|---|
| `weft::latest <cron>` | `weft.latest(cron)` | string | Trimmed stdout of the most recent run |
| `weft::previous <cron>` | `weft.previous(cron)` | string | Stdout of the second-most-recent run (`""` if fewer than 2 exist) |
| `weft::avg <cron> <dur>` | `weft.avg(cron, dur)` | number | Mean of numeric results in window |
| `weft::min <cron> <dur>` | `weft.min(cron, dur)` | number | Smallest numeric result in window |
| `weft::max <cron> <dur>` | `weft.max(cron, dur)` | number | Largest numeric result in window |
| `weft::sum <cron> <dur>` | `weft.sum(cron, dur)` | number | Sum of numeric results in window |
| `weft::stddev <cron> <dur>` | `weft.stddev(cron, dur)` | number | Population stddev; needs ≥2 results |
| `weft::count <cron> <dur>` | `weft.count(cron, dur)` | number | Count of results (rows) in window, numeric or not |
| `weft::percentile <cron> <dur> <p>` | `weft.percentile(cron, dur, p)` | number | Linearly-interpolated p-th percentile, p ∈ [0,100] |
| `weft::first <cron> <dur>` | `weft.first(cron, dur)` | number | Oldest numeric value still in window |
| `weft::delta <cron> <dur>` | `weft.delta(cron, dur)` | number | `latest − first` over window |
| `weft::slope <cron> <dur>` | `weft.slope(cron, dur)` | number | `delta / window_seconds` |
| `weft::rate <cron> <dur>` | `weft.rate(cron, dur)` | number | `sum / window_seconds` |

### Caps

The window caps follow compute cost:

- **Aggregates** (`avg`, `min`, `max`, `sum`, `stddev`, `count`,
  `percentile`, `rate`) cap at **24h** (`1d`). They touch every result in
  the window.
- **Endpoint funcs** (`first`, `delta`, `slope`) cap at **14d**. They
  read only the oldest result in the window and the latest, so a
  longer window is cheap.
- **`latest` / `previous`** are point reads — no window.

### Numeric parsing

Aggregates parse each run's stdout as a float and skip
rows that don't parse. They error when nothing in the window is
numeric (and `weft::stddev` also errors when fewer than 2 numeric
samples exist).

`weft::latest` and `weft::previous` return whatever the cron wrote,
including non-numeric strings. Cast in Lua with `tonumber(...)`.

### Retention

The aggregates read whatever history your plan's retention covers;
older runs are not visible to the helpers.

## Predicates

Predicates return a boolean. In bash that's an exit code (0 = fire,
non-zero = no fire), letting you compose with native `&&` `||` `!`.
In Lua the return is a plain bool, composable with `and` `or` `not`.

On absent data, non-numeric values, or cold-history conditions,
predicates return **false**: when the answer is unknown, the predicate
does not fire. The `alert` builtin behaves the same way on these
conditions. `is_absent` fires when there is no data.

| Bash | Lua | Fires when |
|---|---|---|
| `weft::is_above <cron> <thr> [<dur> [--average]]` | `weft.is_above(cron, thr, [dur, [avg]])` | latest > thr (or min over dur > thr, or avg > thr with `--average`) |
| `weft::is_below <cron> <thr> [<dur> [--average]]` | `weft.is_below(cron, thr, [dur, [avg]])` | latest < thr (or max over dur < thr, or avg < thr with `--average`) |
| `weft::is_absent <cron> [<dur>]` | `weft.is_absent(cron, [dur])` | the cron has no result: it doesn't exist, has never run, or its latest output is empty (with `dur`: no result landed in the last `dur`). A bad or over-cap `dur` (more than 24h, `1d`) or an unreadable history doesn't fire; the bash form says why on stderr |
| `weft::has_changed <cron>` | `weft.has_changed(cron)` | latest ≠ previous tick |
| `weft::is_rising <cron> [<pct> [<dur>]]` | `weft.is_rising(cron, [pct, [dur]])` | (latest − baseline)/baseline ≥ pct% (baseline = prev tick, or window avg if dur given) |
| `weft::is_falling <cron> [<pct> [<dur>]]` | `weft.is_falling(cron, [pct, [dur]])` | (baseline − latest)/baseline ≥ pct% |
| `weft::is_outlier <cron> <n> <dur> [--above\|--below]` | `weft.is_outlier(cron, n, dur, [dir])` | \|latest − avg\| > n·stddev over dur |
| `weft::is_seasonal <cron> <n> <period> <dur>` | `weft.is_seasonal(cron, n, period, dur)` | \|latest − same-time-of-period avg\| > n·stddev |

### Anomaly guards

`is_outlier` and `is_seasonal` both gate against zero-stddev histories:
if the historical stddev drops below `0.01 · |avg|`, the predicate
returns false. `is_seasonal`
additionally requires at least 4 same-time samples.

### Seasonal alignment

For `is_seasonal`, the `period` is the recurring interval and
`window` is how far back to sample. With `period=1d window=4d`, the
predicate compares the latest value to the four values captured at
this same hour on each of the last four days. With `period=1w
window=8w`, it compares to the same hour of the same weekday over
the last eight weeks. Period alignment uses UTC.

## Stdin comparators

To compare an aggregate's stdout with a threshold, pipe it through
one of:

| Bash | Fires when |
|---|---|
| `weft::is_gt <thr>` | stdin parses as float > thr |
| `weft::is_lt <thr>` | stdin parses as float < thr |

```
weft::avg error-rate 5m | weft::is_gt 50
```

In Lua, use the native `>` and `<` operators:
`weft.avg("error-rate", "5m") > 50`.

## See also

- **Crons** — how cron results get persisted and which `weft::*`
  funcs read them back.
- **Alerts** — patterns for composing predicates into
  composite alerts, the multi-row tag-based form, debouncing, and
  outlier vs seasonal guidance.
- **Custom scripts** — using `weft.*` from Lua `/bin/` scripts.
- `man alert` — the alert builtin reference.
