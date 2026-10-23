# Cron output schema

## Status

Proposed. Decisions agreed 2026-10-22: schema from `alert -v/-t` when the cron
alerts (required for tagged or JSON output) and from header comments when it
doesn't, value-first text lines, per-run tag cap by plan, duplicate tags
allowed.

## Problem

A cron run is stored as rows of (run time, tag, value): `cron_results` carries
a text `tag` and a float `value`, and the hourly, four-hour and daily rollups
keep count, sum, sum of squares, min and max per bucket per tag. The storage is
already the two dimensions we want. What is wrong is how a run's output
becomes those rows, and who gets to decide.

- **Only `alert` can make tagged rows.** `persistResults`
  (`coordinator/cron.go`) splits stdout into tagged rows only when the cron
  has an `alert -v <field> -t <field>` stage, stamped on the watch as
  `alert_key`/`alert_tag`. Without one, the whole of stdout must parse as one
  number or the run stores no value. A cron that prints one number per host
  and doesn't alert cannot be charted.
- **Dashboards re-parse stdout.** A sparkline tile's `tag:`/`y:` keys
  (`dashboard/builtin.go`) extract series from raw stdout at draw time,
  ignoring the stored tags and the rollups. Multi-line sparklines only see
  raw history (7 days), and the shape is declared in a third place.
- **`chart <cron>` ignores time.** It fetches the last `-n` raw runs
  (`GetWatchResults`). `-n 50` is four hours of a 5-minute cron and 50 days
  of a daily one, it can't reach past raw retention, it ignores `range`, and
  it rejects tagged crons. The web cron page already has the right query
  (`GET /api/crons/{name}/chart`: window → granularity via `PickGranularity`,
  clamped to retention, per-tag points).
- **No cardinality bound.** Nothing limits how many tags a run emits. Every
  distinct tag is a series with its own raw and rollup rows.

## Goals

- A cron's output alone determines what is stored, whether or not it alerts.
- One declaration of tag and value, read by storage, `chart`, dashboards and
  `alert`.
- Output that breaks the contract is caught at run time and disables the cron
  at once; only running out of time (the run budget, a delivery timeout) is
  struck and benched.
- Storage is bounded by plan limits on every factor.
- `chart <cron>` reads `range`, draws tagged crons as one line per tag, and
  shares the pipe form's flags wherever they mean the same thing.

## Output contract

A cron's schema (which field is the value, which the tag) comes from exactly
one place:

1. **`alert -v/-t`**, when the cron ends in an `alert` stage with either flag
   (as today).
2. **`# value:`/`# tag:` headers**, when it doesn't.
3. **The value-first text rule** below, when there is neither.

A cron with both headers and `alert -v/-t` is rejected at save:
`this cron sets its schema twice (# value: and alert -v); keep one`. Storing
one field while alerting on another takes two crons, the second reading the
first with `weft::*`.

### Text output (no schema)

Each non-blank line is `<value> [<tag>]`, whitespace-separated:

| Line | Stored as |
|---|---|
| `42` | value 42, untagged (tag `""`) |
| `42 api-1` | value 42, tag `api-1` |
| `42 api-1 extra` | strike: more than two fields |
| `n/a api-1` | strike: value is not a number |

Value comes first so that:

- an untagged cron can gain tags in a later version by printing a second
  column, and its untagged history stays one series;
- a line that fails to produce a tag is stored untagged instead of failing the
  run;
- `uniq -c` output (`  42 api-1`) is already in the right shape.

A single-number cron (`wc -l`) is the one-line case and behaves as today.

### JSON output (schema headers)

JSON rows need the cron to say which field is which. A cron that alerts does
that with `alert -v .pct -t .host`; one that doesn't uses header comments next
to `# schedule:`:

```
# schedule: */5 * * * *
# value: .pct
# tag: .host
cat /log/myco/prod/api/host.log | jq -c '{host: $host, pct: .memory.pct}'
```

- `# value:` is required when any schema header is present; `# tag:` is
  optional (rows are untagged without it).
- Both take a `.`-prefixed JSON path. Column numbers are rejected at save
  time: text output has a fixed positional rule and needs no header.
- Each line must be one JSON object. A row whose `# tag:` path is missing or
  null is stored untagged, mirroring the text rule. A row whose `# value:`
  path is missing or not a number is a strike.

Headers are validated at save like `# schedule:` (well-formed paths, `# tag:`
not without `# value:`). Whether the output matches can only be known at run
time.

### Text crons (`# type: text`)

Some crons report state, not a measurement: which coordinator holds a lease,
what version a host runs. `# type: text` declares that a cron's values are
text. It is the only type header; a cron without one stores numbers.

- The output rules are the same with text in the value slot: a text line is
  `<text> [<tag>]`, a JSON row takes its value from `# value:` or `alert -v`.
  More than two fields is still a strike (check 1); a missing value is still a
  strike; a value that is not a number is not.
- Values are stored as text next to the line (`cron_results.text_value`), with
  no numeric value, so they never reach the rollups. Text history is raw only:
  7 days.
- The tag cap applies unchanged.

Readers that need numbers refuse text crons, statically where they can:

| Reader | When it refuses |
|---|---|
| `alert` numeric conditions (`above`, `below`, `eq`, `neq`, `rising`, `falling`) | At save: the stage is in the cron's own file. `any`, `absent`, `changed` work on text. |
| `weft::` numeric functions and predicates | At save when the cron name is a literal; at run time otherwise (a variable, Lua, a `/bin` script). `weft::latest`, `weft::previous`, `weft::describe`, `weft::is_absent`, `weft::has_changed` take text. |
| Dashboard `sparkline`, `counter`, `gauge` tiles | At dashboard save. |
| `chart <cron>` | At invocation, before drawing. |

Saving a cron as `# type: text` is refused while a numeric reader references
it by name (a cron's or `/bin` script's literal call, in bash or Lua, inline
`lua '…'` included, or a numeric dashboard tile), and the refusal lists them.
The run-time check covers only the names a save can't see, and a run that
trips it disables its cron at once instead of striking: it is the static
check arriving late, and every run would fail the same way.

### Table tiles

A `table` tile shows the cron's latest run, one row per tag: tag and value,
numeric or text. Numeric rows sort by value, highest first; text rows by tag.
It reads stored points, not stdout, so `columns:` is gone. A table suits
medium cardinality, where a sparkline legend would fold.

### Duplicate tags

Allowed. Two lines with the same tag in one run are two points at the same run
time. Rollups already handle this (count 2, sum of both, min/max across them).
Readers that want one value per (run, tag) reduce with the chart's aggregate;
the default is `avg`, the same as when points merge to fit the terminal.

## Runtime checks

Each run's output is checked when it is stored. A failing run stores no rows,
so a bad run can't half-populate the rollups, and disables the cron at once
with a page to the `system` alias. A check on the output's shape is a type
check the save couldn't make: a run that fails it, even on one odd log line,
is a bug in the cron, and striking it would let an intermittent one drop
points forever without ever reaching three in a row. Strikes are kept for
time: three runs in a row over the budget or with a timed-out delivery bench
the cron, and a clean run resets the count.

| # | Check | Disable message (draft) |
|---|---|---|
| 1 | Text line has more than two fields (default rule only) | `line 3 has 3 fields; text output is <value> [<tag>]` |
| 2 | Schema uses JSON paths, output not JSON | `line 1 is not JSON, but the cron declares # value: .pct` |
| 3 | Output is JSON, no schema | `output is JSON; add # value: (and # tag:) headers, or alert -v` |
| 4 | Value is not a number | `line 5: value "n/a" is not a number` |
| 5 | More distinct tags than the plan allows | `run printed 140 tags; your plan allows 100 per run` |
| 6 | Tagged text or JSON reaches `alert` without `-v` | `line 1 has a tag; give alert -v 1 -t 2` (or `output is JSON; give alert -v <path>`) |

Empty output stays legal: it stores nothing, as multi-row mode does today.

`consecutive_timeouts` and `RecordCronTimeout` generalize to strikes with a
reason (the run budget or a delivery timeout), so the bench notice says which.

### Tag cap

The cap is per cron, per run: the number of distinct tags one run may print.

| Plan | Tags per run |
|---|---|
| Trial | 10 |
| Swatch | 50 |
| Yard | 100 |
| Bolt | 250 |

Raw rows per day are at most crons × runs per day × tags per run. The plan
already caps cron count and shortest interval, so the per-run cap bounds the
last factor and storage is bounded with one local check. A per-cron cap keeps
one cron's growth from failing another, unchanged cron, which an org-wide pool
would do.

Tag churn (new tags every run, e.g. request IDs) is still bounded in storage by
the same product. Its cost is legibility, which `chart` handles by drawing the
top N tags and folding the rest into `other`.

Enforcement lives in `persistResults`: count the run's distinct tags and
compare with the cap for `tierForWatch(watch)` (`coordinator/cron.go`), one
tier lookup per run.

`crons.md` advises against `head -n`, which is right for input but blocks the
top-N cron that a high-cardinality tag needs
(`… | uniq -c | sort -rn | head -50`). The advice gets narrowed to limiting
input lines.

## Readers

### `chart <cron>`

- **Window from `range`.** Same query as `/api/crons/{name}/chart`: the
  window picks the granularity, clamped to plan retention. `-n` is removed.
- **`--follow` re-resolves `range` on every redraw.** A relative start slides
  (`range 2h..` is always the last two hours); an absolute start stays put and
  the chart grows. A range whose end is in the past has nothing to follow and
  `--follow` is rejected.
- **Tagged crons draw one line per tag** from the stored tags. No `--tag`
  needed.
- **Value is field 1** for text output, per the contract.

### Flag unification

```
chart <cron>       [-a FN] [--bucket DUR] [-t TYPE] [--title TEXT] [-f [-i DUR]]
<pipeline> | chart [-x F] [-y F] [--tag F] [-a FN] [--bucket DUR] [-t TYPE] [--title TEXT]
```

- `-a`, `--bucket` and `--title` gain the cron form. `count`, `sum`, `avg`,
  `min` and `max` are exact at every rollup level. `p50` and `p95` need raw
  runs, so over a window past raw retention they are rejected with an error
  that says so. `--bucket` reads the finest stored granularity that fits.
- `-x`, `-y` and `--tag` stay pipe-only. A cron's x is its run time, and its
  value and tag are fixed when the run is stored.
- `-f` and `-i` stay cron-only. Re-running the upstream pipeline per tick for
  a live pipe chart is possible but costs a shard query per interval; it is
  out of scope here.

### Pipe-form changes

- Default `-y` moves from the last field to field 1, matching the cron
  contract, so a line reads the same in a pipe and in a cron.
- With a time x-axis, the axis and `--bucket` zero-fill span the whole
  `range`, not first-to-last row (`chart/render.go`), so quiet stretches at
  either end show as zeros.

### Dashboards

The tile `tag:` and `y:` keys are removed. A sparkline of a tagged cron is
multi-line, read from the stored tags and rollups like `chart <cron>`. Saving a
dashboard with either key is rejected with a message pointing at the cron's
schema headers.

### `alert`

What a cron stores is what `alert` read: `AlertCommand` copies its input to
stdout whether or not it fires, and in a cron `runAlertStage` leaves the run's
output untouched (only an interactive firing clears it). So moving storage off
`alert -v/-t` changes nothing about which lines get stored, only how they are
split.

`alert`'s conditions fall into two groups:

- **Stateless**: `any`, `above`, `below`, `eq`, `neq` without `--duration`.
  They look only at the current line.
- **History**: `changed`, `rising`, `falling`, `absent --duration`, and
  `above`/`below` with `--duration`. They read the cron's *stored* rows for
  the same tag (`ListCronResults` for the previous tick, `cron_results_all`
  for window stats).

A history condition is only meaningful if `alert`'s value and tag are the
stored value and tag. Keeping `alert -v/-t` as the storage schema (source 1
above) makes that true by construction: there is never a second declaration
to disagree with.

- `alert -v/-t` keep their meaning in the shell and in crons, and in a cron
  they remain the schema (`alert_key`/`alert_tag`, as today).
- A cron with an `alert` stage must give `-v` (and `-t` for tags) whenever
  its output is tagged text or JSON. `alert` with no flags keeps today's
  meaning: the run's output is one untagged value. Tagged lines or JSON
  reaching a flagless `alert` is a strike (check 6); output shape is only
  known at run time.
- Headers and `alert -v/-t` never combine (rejected at save, above), so a
  cron that alerts declares its schema only with flags.
- The two-field limit (check 1) applies only to the default text rule, so
  `alert -v 3 -t 1` on five-column output stores field 3 tagged by field 1.
- `alert` in the shell is unchanged.

### Existing `alert` bugs

Found while checking the above, each proven by a failing test in
`coordinator/alert_history_test.go`:

- **History is one tick stale.** `alert` runs before `persistResults` stores
  the current tick, so `ListCronResults(id, 2, tag)[0]` is already the previous
  tick, but the code reads `[1]`, the tick before that. `rising`, `falling`
  and `changed` compare against two ticks ago. 10 → 100 → 110 fires
  `rising 50%` as "rose 1000% (from 10.0 to 110.0)".
- **`changed` compares a value to a line.** In multi-row mode it compares the
  current row's extracted value (`5`) to the previous row's whole stdout
  (`a 5`), so a tagged `changed` fires on every tick.

Both are independent of this plan and can be fixed first.

## Open questions

- **`weft::*` with tags.** `weft::latest` returns the stdout of the newest row
  (`coordinator/weftfuncs.go`), which for a tagged cron is an arbitrary tag.
  The predicates and aggregates need a tag argument or a documented reduction.
- **Top-N fold in `chart`.** The value of N, and whether dashboards fold the
  same way.

## Migration

No paying customers, so no compatibility layer. Existing crons that relied on
`alert -v/-t` for multi-row storage are rewritten to value-first text or schema
headers in the same change. Crons whose stdout is a single number are
unaffected. Dogfood dashboards that use `tag:`/`y:` move to schema headers on
their crons.

## Docs

On ship:

- `crons.md`: the output contract and schema headers; the six checks in
  Runtime analysis; the narrowed `head -n` advice; the `tag` row of the result
  table.
- `dashboards.md`: `chart <cron>` reads `range`, the `--follow` rule, the
  unified flags, tagged crons as one line per tag, removal of tile
  `tag:`/`y:`.
- `alerts.md`: in a cron, `-v`/`-t` are the stored schema, are required for
  tagged or JSON output, and can't be combined with headers. Also fix the
  existing claim that stdin passes through only when the condition doesn't
  hold: in a cron it always does.

## Order of work

0. Fix the two `alert` history bugs (tests already written).
1. Output parsing and the runtime checks in `persistResults`, with strikes
   generalized from timeouts. Tag cap by plan.
2. `chart <cron>` on the windowed rollup query; `range` and `--follow`;
   `-n` removed; per-tag lines.
3. `-a`, `--bucket`, `--title` on the cron form.
4. Pipe-form default `-y` and full-range x-axis.
5. Dashboards read stored tags; tile `tag:`/`y:` removed.
6. Rewrite dogfood crons and dashboards; docs.
