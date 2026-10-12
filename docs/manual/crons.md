---
id: manual-crons
title: Crons
web:
  section: crons
  category: alert-visualize
  order: 50
---

## Set up a cron

Crons are saved pipelines that run on a schedule. Create one from
the shell:

```
# Open the cron editor
vi /cron.d/error-count

# Cron file format: a '# schedule:' directive plus a pipeline.
# schedule: */5 * * * *
cat /log/myco/prod/api/main.log | grep ERROR | wc -l | alert above 0 --notify=ops
```

The schedule is either a five-field expression (`*/5 * * * *`) or
`every <duration>` (`# schedule: every 5m`). Each run reads only the
logs from its own schedule interval: a `*/5` cron sees the last five
minutes, so the pipeline above counts that interval's errors with no
row limit. Don't put `head -n` or `tail -n` in a cron. A line count caps
the rows read, and on a busy interval it silently cuts the window short, so
the cron undercounts without telling you. To try a cron's pipeline at the
prompt first, set the window with `range` (see `man range`).

The pipeline's stdout is captured as the cron result — numeric
values are graphed on the dashboard as sparklines. Alerts are
dispatched by the `alert` stage (see **Alerting strategies** for
the catalogue of alert shapes, plus `man alert` for the builtin's
grammar); there is no separate cron-level notify field.

View and manage crons from the **Dashboard**.

### Accessing past results

Every cron run is stored. The `weft::*` family pulls summaries out
of that history (aggregates like `weft::avg`, point reads like
`weft::latest`) and a parallel predicate family (`weft::is_above`
etc.) exits 0 (fire) or 1 (don't fire) for use in composite alerts. They're
available at the shell prompt, inside `/bin/` scripts, and as
pipeline stages in other crons:

```
# Interactively, at the prompt
weft::latest error-rate

# Chained through a pipeline
echo $(weft::avg error-rate 1h) | alert above 50 --notify=ops
```

From a Lua `/bin/` script the return type is a number, with no string
parsing:

```lua
local rate = weft.avg("error-rate", "1h")
```

Windows are Go durations plus `d` (a day) and `w` (a week), and they
combine: `30m`, `1h`, `1d`, `1h30m`. Aggregates reach back at most 24
hours (`1d`).

The full reference (value funcs, predicates, stdin comparators,
caps, numeric-parsing rules) is in **Weft functions**. Patterns
that combine these into alerts of various shapes — multi-row
per-tag, composite, anomaly-based — live in **Alerting strategies**.

Tier limits (number of crons and minimum schedule interval) vary by
plan — see the **Account** page for your current caps, or run
`help cron` in the shell.

### Cron version history

Every save to `/cron.d/<name>` is captured. After `:w`, the shell
prompts for an optional commit message; `git log -- /cron.d/<name>`
lists past versions, and `git restore --source=HEAD~2 /cron.d/<name>`
makes an old version current again by saving it as a new version. The
live version plus the five before it are kept. See **git** for the full
surface.
