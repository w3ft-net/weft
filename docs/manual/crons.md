---
id: manual-crons
title: Crons
web:
  section: crons
  category: analysis-alerting
  order: 50
---

In weft, crons have two primary purposes: generating summary timeseries data
based on logs and metrics, and alerting on such data. Creating such a cron is
as simple as adding a bash or Lua script to `/cron.d`. Those scripts need a
leading comment indicating the schedule on which the cron will fire.

Let's take the following script as an example:

```
# schedule: */5 * * * *
cat /log/myco/prod/api/main.log | grep ERROR | wc -l | alert above 0 --notify=ops
```

## Scheduling

There are two options for cron schedule syntax: OG crontab syntax, and an
interval syntax. Crons fire on fixed intervals at round UTC times. Below are
the accepted values for each:

  | crontab | interval |
  |---|---|
  | `* * * * *` | every minute |
  | `*/N * * * *` | every N minutes |
  | `0 * * * *` | every hour |
  | `0 */H * * *` | every H hours |
  | `0 0 * * *` | every day, at 00:00 UTC |
  | `0 0 */N * *` | every N days |
  | `0 0 * * 1` | every week, on Monday at 00:00 UTC |

Weft will refuse to save a file in `/cron.d` if the schedule line is missing or
fails to conform to the scheduling options above.

Weft sets the `range` for each run automatically based on the cron's schedule
interval: a `*/5` cron sees the last five minutes, a daily cron sees the
UTC day that just ended, and a weekly cron sees the week that just ended.
For this reason, we recommend against using `head -n`, `tail -n`, etc.
Imposing a limit on your lines might result in undercounting or other missed
line problems.

### Limits on granularity

The shortest interval you can schedule depends on your plan, and so does
the number of crons you can keep:

| Plan | Shortest interval | Crons |
|---|---|---|
| Trial | 5 minutes | 1 |
| Swatch | 5 minutes | 20 |
| Yard | 1 minute | 100 |
| Bolt | 1 minute | 500 |

Saving a cron with a schedule faster than your plan allows is rejected.

Disabled crons (see [Runtime analysis](crons.md#runtime-analysis)) don't count
against the number of crons your plan allows.

## Cron result persistence

Since your cron monitors are just bash/Lua scripts against your logs, it's very
easy to test them out since you have a shell that executes in the exact same
way. But once you are done tweaking and get them saved to `/cron.d`, every cron
run gets persisted. After they've been running for a bit, you'll have a
portrait of what the runs look like over time. 

```
weft::describe error-rate | jq -r 'select(.exit_code != 0) | .ran_at + " " + .stderr'
```

The output of this function is NDJSON with the following values:

| Field | Contents |
|---|---|
| `stdout` | The pipeline's output, exactly as written |
| `stderr` | Alert firings (from an `alert` stage) or the error message if the run failed |
| `exit_code` | `0`, or `1` if the pipeline errored |
| `duration_ms` | How long the run took, in milliseconds |
| `ran_at` | When the run started |
| `value` | `stdout` parsed as a number, or empty if it isn't one |
| `tag` | The per-row label of a multi-row cron (see below), empty otherwise |

We'll cover more ways of analyzing cron data below.

### Retention

Raw results are rolled up into progressively coarser summaries as they age.
Every summary stores the count, sum, sum of squares, and minimum and maximum
per tag, so averages, minima, maxima and standard deviations stay exact when
buckets are merged.  Only runs with a numeric `value` are summarised; text-only
results exist in the raw history alone.

| Granularity | Kept for |
|---|---|
| Raw results (every run, full `stdout` and `stderr`) | 7 days |
| Hourly summary | 14 days |
| Four-hour summary | 35 days |
| Daily summary | 400 days |

## Analyzing cron data

The `weft::*` family of functions reads your raw cron history from inside a
script. They come in three flavors:

- **Aggregates** return the most recent value, an aggregate over a
  window, or an anchor like the slope between two endpoints.
- **Predicates** return a boolean — "is this cron above 50?",
  "did it just change?"
- **Comparators** turn a piped value into a boolean.

With the exception of some comparators, each function is available in both bash
and Lua.  Bash uses `::` as the namespace separator, and Lua uses `.`. The
arguments are the same in both.

For composing predicates into composite alerts, see
[Alerts](alerts.md).

### Aggregates

All aggregates take a cron name and (with the exception of `weft::latest` and
`weft::previous`) a duration window. Durations are a number and a unit: `30s`,
`5m`, `1h`, `6h`, plus `d` for a day (24 hours) and `w` for a week (7 days).
Units combine (e.g. `1d12h`). Some functions will truncate your duration to the
raw data window (7 days) because the math doesn't work on the compressed cron
results. For all others, the limiting factor is your plan's retention.

The return type in the table below indicates not just what the function spits
out but what it expects in the cron results -- if the stdout from the cron
argument isn't coercible to a numeric value, the functions in the table below
aren't going to work.

| Bash | Lua | Returns | Longest window | Notes |
|---|---|---|---|---|
| `weft::avg <cron> <dur>` | `weft.avg(cron, dur)` | number | 400d | Mean of numeric results in window |
| `weft::count <cron> <dur>` | `weft.count(cron, dur)` | number | 400d | Count of numeric results in window |
| `weft::delta <cron> <dur>` | `weft.delta(cron, dur)` | number | 7d | `latest − first` over window |
| `weft::first <cron> <dur>` | `weft.first(cron, dur)` | number | 7d | Oldest numeric value still in window |
| `weft::latest <cron>` | `weft.latest(cron)` | string | n/a | Trimmed stdout of the most recent run |
| `weft::max <cron> <dur>` | `weft.max(cron, dur)` | number | 400d | Largest numeric result in window |
| `weft::min <cron> <dur>` | `weft.min(cron, dur)` | number | 400d | Smallest numeric result in window |
| `weft::percentile <cron> <dur> <p>` | `weft.percentile(cron, dur, p)` | number | 7d | Linearly-interpolated p-th percentile, p ∈ [0,100] |
| `weft::previous <cron>` | `weft.previous(cron)` | string | n/a | Stdout of the second-most-recent run (`""` if fewer than 2 exist) |
| `weft::rate <cron> <dur>` | `weft.rate(cron, dur)` | number | 400d | `sum / window_seconds` |
| `weft::slope <cron> <dur>` | `weft.slope(cron, dur)` | number | 7d | `delta / window_seconds` |
| `weft::stddev <cron> <dur>` | `weft.stddev(cron, dur)` | number | 400d | Population stddev; needs ≥2 results |
| `weft::sum <cron> <dur>` | `weft.sum(cron, dur)` | number | 400d | Sum of numeric results in window |

### Predicates

The following predicate functions are available:

| Bash | Lua | True when |
|---|---|---|
| `weft::is_above <cron> <thr> [<dur> [--average]]` | `weft.is_above(cron, thr, [dur, [avg]])` | latest > thr (or min over dur > thr, or avg > thr with `--average`) |
| `weft::is_below <cron> <thr> [<dur> [--average]]` | `weft.is_below(cron, thr, [dur, [avg]])` | latest < thr (or max over dur < thr, or avg < thr with `--average`) |
| `weft::is_absent <cron> [<dur>]` | `weft.is_absent(cron, [dur])` | the cron has no result: it doesn't exist, has never run, or its latest output is empty (with `dur`: no result landed in the last `dur`). A bad or over-cap `dur` (more than 7d) or an unreadable history is false; the bash form says why on stderr |
| `weft::has_changed <cron>` | `weft.has_changed(cron)` | latest ≠ previous tick |
| `weft::is_rising <cron> [<pct> [<dur>]]` | `weft.is_rising(cron, [pct, [dur]])` | (latest − baseline)/baseline ≥ pct% (baseline = prev tick, or window avg if dur given) |
| `weft::is_falling <cron> [<pct> [<dur>]]` | `weft.is_falling(cron, [pct, [dur]])` | (baseline − latest)/baseline ≥ pct% |
| `weft::is_outlier <cron> <n> <dur> [--above\|--below]` | `weft.is_outlier(cron, n, dur, [dir])` | \|latest − avg\| > n·stddev over dur |
| `weft::is_seasonal <cron> <n> <period> <dur>` | `weft.is_seasonal(cron, n, period, dur)` | \|latest − same-time-of-period avg\| > n·stddev |

#### Truthiness

In bash a predicate reports its result as an exit code, and in Lua as a plain bool.

| Situation | Exit code | Lua |
|---|---|---|
| Condition holds | `0` (true) | `true` |
| Condition doesn't hold | `1` (false) | `false` |
| Can't evaluate: the cron doesn't exist, its output isn't numeric, or there isn't enough history yet | `1` (false) | `false` |
| Bad arguments (a missing argument, an unparseable threshold or duration) | `2` | error raised on bad argument types, unparseable duration returns `false` |

Because "no" and "can't tell" both exit 1, `!` turns an unknown into true. Rather than negating, use the opposite predicate, and pair it with `weft::is_absent` to catch a cron that has stopped reporting.

#### Anomaly guards

`is_outlier` and `is_seasonal` both gate against zero-stddev histories:
if the historical stddev drops below `0.01 · |avg|`, the predicate
returns false. `is_seasonal`
additionally requires at least 4 same-time samples.

#### Seasonal alignment

For `is_seasonal`, the `period` is the recurring interval and
`window` is how far back to sample. With `period=1d window=4d`, the
predicate compares the latest value to the four values captured at
this same hour on each of the last four days. Period alignment uses UTC.

Because the window is limited to the 7 days of raw results (see the table
above), a weekly period can never gather the 4 samples it needs, so
`is_seasonal` is only useful with periods of a day or less.

### Stdin comparators

Lua has comparison operators that everyone would expect, so you can write
`weft.avg('error-rate', '5m') > 50` and everyone knows what you mean. The
unique beauty of bash adds a tasty bit of enigma:

```
[ "$(weft::avg error-rate 5m)" -gt 50 ]
```

And while we love bash's quirky conditionals, they do not compose nicely in
pipelines. Furthermore, bash comparisons only work on integers, whereas weft's
aggregate functions return floats. Rather than ask you to pipe to `bc`, we
provide a couple convenience functions that can handle numbers with dots in them:

| Bash | True when |
|---|---|
| `weft::is_gt <thr>` | stdin parses as float > thr |
| `weft::is_lt <thr>` | stdin parses as float < thr |

Thus, instead of the previous example, you can do:

```
weft::avg error-rate 5m | weft::is_gt 50
```

Both comparators treat empty or non-numeric input as false.

## Static analysis

Whenever you write a cron, weft analyzes its contents for a few types of correctness:

* **Schedule** -- a schedule line must be present, confirm to the [accepted
  grammar](crons.md#scheduling), and fire at a frequency allowed by your
organization's plan
* **Syntax** -- the script must parse as syntactically valid bash (and awk,
  sed, or jq if using those interpreters)
* **Aggregates** -- `weft::` aggregate functions read stored history and ignore
  their input, so if they follow a pipe, you're doing something wrong
* **Execution limits** -- `tail -f` is disallowed, since a script has to run to
  completion, as is `range`, since the range of a cron is determined by its
schedule

If you run afoul of any of these rules, weft will refuse to write the contents
to `/cron.d` (or `/bin`).

## Runtime analysis

Weft also watches how your crons behave once they are running. Each run has a
budget of 2 seconds. A run that exceeds it is cancelled and recorded with the
status `timeout`, and no further output from that run is saved.

A notification target that doesn't accept an alert within 10 seconds also
counts as a timeout for the cron that raised it (see [Delivery
timeouts](alerts.md#delivery-timeouts)); it doesn't count against the 2
seconds above.

A cron that times out three ticks in a row, in either way, is benched: weft
disables it, so it stops firing and stops counting against your plan's cron
limit. A tick with no timeout resets the count. When weft benches a cron it
notifies your [`system` alias](alerts.md#the-system-alias), naming the cron and
saying how to turn it back on.

A disabled cron stays in `/cron.d` with a marker at the top of its file:

```
# schedule: */5 * * * *
# enabled: false
cat /log/myco/prod/api/main.log | grep ERROR | wc -l
```

You can also write `# enabled: false` yourself to pause a cron. To turn it back
on, save the file without that line (or with `# enabled: true`). Re-enabling
starts the count over, and is refused if you are already at your plan's cron
limit.

Before re-enabling a benched cron, narrow what it reads. A cron over a smaller
`range` or a more specific log path will finish well inside its budget.
