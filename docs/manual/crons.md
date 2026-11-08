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
as simple as adding a bash script to `/cron.d`. Those scripts need a leading
comment indicating the schedule on which the cron will fire.

Let's take the following script as an example:

```
# schedule: */5 * * * *
cat /log/myco/prod/api/main.log | grep ERROR | wc -l
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
interval: a `*/5` cron sees the last five minutes, a daily cron sees the UTC
day that just ended, and a weekly cron sees the week that just ended.  For this
reason, we recommend against using `head -n`, `tail -n`, etc.  Imposing a limit
on your lines might resulting in undercounting or other missed line problems.

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
| `stdout` | The line the point came from |
| `stderr` | Alert firings (from an `alert` stage), or why the run failed or struck |
| `exit_code` | `0`, or `1` if the run failed or struck |
| `duration_ms` | How long the run took, in milliseconds |
| `ran_at` | When the run started |
| `value` | The point's value, empty for a text cron |
| `text` | A [text cron](crons.md#declaring-text-output)'s value, absent otherwise |
| `tag` | The point's tag, empty if untagged |

A run that prints several points has one entry per point. A run that fails
has a single entry, with no value, with the reason in `stderr`.

We'll cover more ways of analyzing cron data below.

### Type declarations

Crons have a simple type system that helps weft guide your scripts. Whatever
can be enforced statically is, but much of the type checking is done at
runtime. By default, a cron is assumed to return one numeric value per line,
with an optional tag following whitespace. You can declare richer types with
comments at the top of the file. With or without explicit type declarations, a
cron that fails runtime analysis will be automatically disabled. Here's how
weft analyzes stdout for a cron with no type declarations:

| Line |  Outcome |
|---|---|
| `42` | value 42 is stored, no tag |
| `42 api-1` | value 42 is stored, tagged `api-1` |
| `42 api-1 extra` | fails analysis for having more than two fields, cron disabled |
| `n/a api-1` | fails analysis for non-numeric first field, cron disabled |

#### Declaring text output

If you've got a series of strings in your logs you want to roll up in some way,
you can, you just have to tell weft that you're expecting text. For example:

```
# schedule: */5 * * * *
# type: text
cat /log/myco/prod/api/main.log | grep 'starting version' \
    | awk '{v[HOST]=$NF} END {for (h in v) print v[h], h}'
```

Downstream of this cron, `alert`, `chart`, `dashboard`, and aggregate functions
will do typechecking and disallow certain modes based on this type declaration.

#### Extracting values and tags from JSON

If the stdout from your cron is JSON, you need to tell weft what you'd like
persisted in the `value` and `tag` fields. You can do so with comments
indicating the JSON path:

```
# schedule: */5 * * * *
# value: .pct
# tag: .host
cat /log/myco/prod/api/host.log | jq -c '{host: $host, pct: .memory.pct}'
```

A `type: text` declaration applies to whatever JSON path you've pointed `value`
at if you want to extract a string from an object.

### Plan limits on crons

Number of executing crons, scheduling granularity, and tag count are limited by your plan:

| Plan | Shortest interval | Crons | Tags per run |
|---|---|---|---|
| Trial | 5 minutes | 1 | 10 |
| Swatch | 5 minutes | 20 | 50 |
| Yard | 1 minute | 100 | 100 |
| Bolt | 1 minute | 500 | 250 |

Weft won't let you save a cron with a schedule more frequent than your plan allows.
It will also disable a cron that produces more tags than your plan limit.

Disabled crons (see [Runtime analysis](crons.md#runtime-analysis)) don't count
against your plan limit.

### Older data and your history budget

Recent data (the last 14 days) is always available. Reading older data counts
against a monthly history budget, in lines read:

| Plan | Older data read per month |
|---|---|
| Trial | none |
| Swatch | 34 million lines |
| Yard | 1.6 billion lines |

`cat /etc/usage` shows how much of it you have used. When it is used up, a query
that needs older data fails with a message saying so and when the month resets;
queries over recent data are not affected.

A cron fires over a window as long as its schedule, so a cron that looks back
nearly 14 days can reach older data. Saving one warns you. If a fire needs older
data after the budget is used up, it fails, weft disables the cron, and a
notice goes to your `system` alias. To turn it back on, shorten the schedule or
wait for the budget to reset, then edit `/cron.d/<name>` and remove the
`# enabled: false` line.

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

Depending on the context, weft will verify either statically or at runtime that
its first argument is a cron and returns a type that matches the signature of
the aggregate. 

| Bash | Lua | Accepts/Returns | Longest window | Notes |
|---|---|---|---|---|
| `weft::avg <cron> <dur>` | `weft.avg(cron, dur)` | number | 400d | Mean of numeric results in window |
| `weft::count <cron> <dur>` | `weft.count(cron, dur)` | number | 400d | Count of numeric results in window |
| `weft::delta <cron> <dur>` | `weft.delta(cron, dur)` | number | 7d | `latest − first` over window |
| `weft::first <cron> <dur>` | `weft.first(cron, dur)` | number | 7d | Oldest numeric value still in window |
| `weft::latest <cron>` | `weft.latest(cron)` | text/number | n/a | Trimmed stdout of the most recent run |
| `weft::max <cron> <dur>` | `weft.max(cron, dur)` | number | 400d | Largest numeric result in window |
| `weft::min <cron> <dur>` | `weft.min(cron, dur)` | number | 400d | Smallest numeric result in window |
| `weft::percentile <cron> <dur> <p>` | `weft.percentile(cron, dur, p)` | number | 7d | Linearly-interpolated p-th percentile, p ∈ [0,100] |
| `weft::previous <cron>` | `weft.previous(cron)` | text/number | n/a | Stdout of the second-most-recent run (`""` if fewer than 2 exist) |
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

* **Schedule** -- a schedule line must be present, conform to the [accepted
  grammar](crons.md#scheduling), and fire at a frequency allowed by your
  organization's plan
* **Syntax** -- the script must parse as syntactically valid bash (and awk,
  sed, jq or Lua if using those interpreters)
* **Aggregates** -- beyond type signatures, `weft::` aggregate functions read
  stored history and ignore their input, so if they follow a pipe, you're doing
  something wrong
* **Execution limits** -- `tail -f` is disallowed, since a script has to run to
  completion, as is `range`, since the range of a cron is determined by its
  schedule
* **Output schema** -- `# tag:` cannot be set without `# value:`
* **Types** -- weft will enforce correct typing of cron arguments to aggregate
  and predicate functions

If you run afoul of any of these rules, weft will refuse to write the contents
to `/cron.d` (or `/bin`).

## Runtime analysis

Weft also watches how your crons behave once they are running. Each run has a
budget of 2 seconds. A run that exceeds it is cancelled and recorded with the
status `timeout`, and no further output from that run is saved. Three timeouts
in a row disable the cron; a run that finishes in time resets the count.

A notification target that doesn't accept an alert within 10 seconds counts
toward those three as well (see [Key behaviors](alerts.md#key-behaviors)).

Each run's output is evaluated against [its type
declaration](crons.md#type-declarations). A run whose output breaks the rules
is recorded with the status `invalid` and the reason in its `stderr`, and is
disabled.

A type mismatch between aggregate or predicate functions and their cron
argument is normally checked statically, but in cases where the mismatch is
known only at runtime by passing a variable, the  cron will be disabled then. 

As described above, type declarations are verified and enforced, including JSON
extraction. Empty output is treated as conformant.

Type/parsing related runtime failures result in the cron being disabled
immediately.  A disabled cron stays in `/cron.d` with a marker at the top of
its file:

```
# schedule: */5 * * * *
# enabled: false
cat /log/myco/prod/api/main.log | grep ERROR | wc -l
```

You can also write `# enabled: false` yourself to pause a cron. To turn it back
on, save the file without that line (or with `# enabled: true`). Re-enabling
starts the count over, and is refused if you are already at your plan's cron
limit.
