---
id: manual-weft-funcs
title: Weft functions
web:
  section: weft-funcs
  category: customize
  order: 52
---

The `weft::*` family is the surface for reading and reasoning about
your own watch history from inside a pipeline. There are three
shapes:

- **Value funcs** return the most recent value, an aggregate over a
  window, or an anchor like the slope between two endpoints.
- **Predicates** return a boolean — "is this watch above 50?",
  "did it just change?" — for composing alert conditions across
  multiple watches.
- **Stdin comparators** turn a piped value into a boolean.

The same set is exposed in two host languages:

| Surface | Syntax | Example |
|---|---|---|
| Bash builtin | `weft::name args` | `weft::avg error-rate 1h` |
| Lua function | `weft.name(args)` | `weft.avg("error-rate", "1h")` |

Bash uses `::` as the namespace separator; Lua uses `.` for table
access. The argument shapes are otherwise identical, and the same Go
implementation backs both — there's no drift to worry about.

For composing predicates into composite alerts, see
**Alerting strategies**.

## Value functions

Each takes a watch name, returns a value, and (with the exception of
`weft::latest` and `weft::previous`) a duration window. Durations use
Go syntax: `30s`, `5m`, `1h`, `6h`, `24h`.

| Bash | Lua | Returns | Notes |
|---|---|---|---|
| `weft::latest <w>` | `weft.latest(w)` | string | Trimmed stdout of the most recent run |
| `weft::previous <w>` | `weft.previous(w)` | string | Stdout of the second-most-recent run (`""` if &lt;2 exist) |
| `weft::avg <w> <dur>` | `weft.avg(w, dur)` | number | Mean of numeric results in window |
| `weft::min <w> <dur>` | `weft.min(w, dur)` | number | Smallest numeric result in window |
| `weft::max <w> <dur>` | `weft.max(w, dur)` | number | Largest numeric result in window |
| `weft::sum <w> <dur>` | `weft.sum(w, dur)` | number | Sum of numeric results in window |
| `weft::stddev <w> <dur>` | `weft.stddev(w, dur)` | number | Population stddev; needs ≥2 results |
| `weft::count <w> <dur>` | `weft.count(w, dur)` | number | Count of runs in window (numeric or not) |
| `weft::percentile <w> <dur> <p>` | `weft.percentile(w, dur, p)` | number | Linearly-interpolated p-th percentile, p ∈ [0,100] |
| `weft::first <w> <dur>` | `weft.first(w, dur)` | number | Oldest numeric value still in window |
| `weft::delta <w> <dur>` | `weft.delta(w, dur)` | number | `latest − first` over window |
| `weft::slope <w> <dur>` | `weft.slope(w, dur)` | number | `delta / window_seconds` |
| `weft::rate <w> <dur>` | `weft.rate(w, dur)` | number | `sum / window_seconds` |

### Caps

The window caps encode compute-cost shape, not arbitrary policy:

- **Aggregates** (`avg`, `min`, `max`, `sum`, `stddev`, `count`,
  `percentile`, `rate`) cap at **24h**. They touch every result in
  the window.
- **Endpoint funcs** (`first`, `delta`, `slope`) cap at **14d**. They
  read O(1) anchor rows — the oldest in-window plus the latest — so
  a longer window is cheap.
- **`latest` / `previous`** are point reads — no window.

### Numeric parsing

Aggregates parse each run's stdout as a float and silently skip
rows that don't parse. They error when nothing in the window is
numeric (and `weft::stddev` also errors when fewer than 2 numeric
samples exist).

`weft::latest` and `weft::previous` return whatever the watch wrote,
including non-numeric strings. Cast in Lua with `tonumber(...)`.

### Retention

The aggregates read whatever history your plan's retention covers;
older runs are invisible to the helpers even if they're still on
disk.

## Predicates

Predicates return a boolean. In bash that's an exit code (0 = fire,
non-zero = no fire), letting you compose with native `&&` `||` `!`.
In Lua the return is a plain bool, composable with `and` `or` `not`.

On absent data, non-numeric values, or cold-history conditions,
predicates return **false** rather than erroring — "we can't say
yes, so it doesn't fire" matches how the `alert` builtin behaves
on the same conditions.

| Bash | Lua | Fires when |
|---|---|---|
| `weft::is_above <w> <thr> [<dur> [--average]]` | `weft.is_above(w, thr, [dur, [avg]])` | latest > thr (or min over dur > thr, or avg > thr with `--average`) |
| `weft::is_below <w> <thr> [<dur> [--average]]` | `weft.is_below(w, thr, [dur, [avg]])` | latest < thr (or max over dur < thr, or avg < thr with `--average`) |
| `weft::is_absent <w> [<dur>]` | `weft.is_absent(w, [dur])` | latest is empty (or no result landed in dur) |
| `weft::has_changed <w>` | `weft.has_changed(w)` | latest ≠ previous tick |
| `weft::is_rising <w> [<pct>] [<dur>]` | `weft.is_rising(w, [pct], [dur])` | (latest − baseline)/baseline ≥ pct% (baseline = prev tick, or window avg if dur given) |
| `weft::is_falling <w> [<pct>] [<dur>]` | `weft.is_falling(w, [pct], [dur])` | (baseline − latest)/baseline ≥ pct% |
| `weft::is_outlier <w> <n> <dur> [--above\|--below]` | `weft.is_outlier(w, n, dur, [dir])` | \|latest − avg\| > n·stddev over dur |
| `weft::is_seasonal <w> <n> <period> <dur>` | `weft.is_seasonal(w, n, period, dur)` | \|latest − same-time-of-period avg\| > n·stddev |

### Anomaly guards

`is_outlier` and `is_seasonal` both gate against zero-stddev histories:
if the historical stddev drops below `0.01 · |avg|`, the predicate
returns false rather than firing on any tiny departure. `is_seasonal`
additionally requires at least 4 same-time samples.

### Seasonal alignment

For `is_seasonal`, the `period` is the recurring interval and
`window` is how far back to sample. With `period=1d window=4w`, the
predicate compares the latest value to the four values captured at
this same hour on each of the last four days. With `period=1w
window=8w`, it compares to the same hour of the same weekday over
the last eight weeks. Period alignment uses UTC.

## Stdin comparators

For windowed *value* comparisons — taking an aggregate's stdout and
checking it against a threshold — pipe through one of:

| Bash | Fires when |
|---|---|
| `weft::is_gt <thr>` | stdin parses as float > thr |
| `weft::is_lt <thr>` | stdin parses as float < thr |

```
weft::avg error-rate 5m | weft::is_gt 50
```

In Lua you'd just write `weft.avg("error-rate", "5m") > 50` — the
native `>` / `<` operators do the same job, no helper needed.

## See also

- **Watches** — how watch results get persisted and which `weft::*`
  funcs read them back.
- **Alerting strategies** — patterns for composing predicates into
  composite alerts, the multi-row tag-based form, debouncing, and
  outlier vs seasonal guidance.
- **Custom scripts** — using `weft.*` from Lua `/bin/` scripts.
- `man alert` — the alert builtin reference.
